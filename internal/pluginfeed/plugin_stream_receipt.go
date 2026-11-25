package pluginfeed

import (
	"encoding/json"
	"maps"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/statefile"
)

func validStreamDependency(dependency streamDependency) bool {
	guard := dependency.Guard
	if dependency.Class != "active" && dependency.Class != "muted" && dependency.Class != "absent" {
		return false
	}
	if dependency.Class == "absent" && guard == (streamGuard{}) {
		return true
	}
	if !model.IsCanonicalUUID(guard.Generation) || !validPluginSHA(guard.Owner) {
		return false
	}
	if guard.Audit != "" && !model.IsCanonicalUUID(guard.Audit) {
		return false
	}
	return dependency.Class != "active" || model.IsCanonicalUUID(guard.Audit)
}

func validateStreamReceiptEvidence(state pluginStreamState, pending *streamPending) error {
	if err := validateStreamGuardSet(pending.Guards); err != nil {
		return err
	}
	var frame streamFrame
	if err := json.Unmarshal(pending.Frames[0], &frame); err != nil {
		return err
	}
	switch pending.Kind {
	case "gap":
		return validateStreamGapReceipt(pending)
	case "event":
		return validateStreamEventReceipt(state, pending, *frame.Event)
	default:
		return statefile.ErrInvalidFile
	}
}

func validateStreamGuardSet(guards map[string]streamDependency) error {
	for id, dependency := range guards {
		if model.ValidateSessionAddress(id) != nil || model.IsPluginInboxAddress(id) || !validStreamDependency(dependency) {
			return statefile.ErrInvalidFile
		}
	}
	return nil
}

func validateStreamGapReceipt(pending *streamPending) error {
	if pending.Retirement != nil || len(pending.Guards) != 0 || pending.SessionID != "" || pending.SessionSeen != nil || pending.MessageSeen != nil || pending.ResetAfter != "" {
		return statefile.ErrInvalidFile
	}
	return nil
}

func validateStreamEventReceipt(state pluginStreamState, pending *streamPending, event model.Event) error {
	if (pending.ResetAfter != "" && (pending.Retirement == nil || pending.ResetAfter != "membership")) || !model.IsCanonicalUUID(pending.After.Epoch) || pending.After.Sequence == 0 {
		return statefile.ErrInvalidFile
	}
	if event.Message == nil {
		return validateStreamSessionReceipt(state, pending, event)
	}
	return validateStreamMessageReceipt(state, pending, *event.Message)
}

func validateStreamSessionReceipt(state pluginStreamState, pending *streamPending, event model.Event) error {
	dependency, exists := pending.Guards[event.Session.ID]
	proof := pending.SessionSeen
	eligible := streamSessionEventEligible(event.Session, dependency.Guard, true)
	if pending.Retirement != nil {
		prior, seen := state.States[event.Session.ID]
		eligible = validStreamRetirement(pending.Retirement, event) && dependency.Guard == pending.Retirement.Before && seen && !prior.Retired && prior.Guard == pending.Retirement.Before
	}
	if !matchingStreamSessionReceipt(pending, event.Session, dependency, exists, eligible) {
		return statefile.ErrInvalidFile
	}
	if proof.Guard != dependency.Guard || proof.State != event.Session.State || proof.Retired != event.Session.Retired {
		return statefile.ErrInvalidFile
	}
	return validateStreamSessionHistory(state.States, event.Session, dependency.Guard)
}

func matchingStreamSessionReceipt(pending *streamPending, session model.EventSession, dependency streamDependency, exists, eligible bool) bool {
	return len(pending.Guards) == 1 && exists && dependency.Class == "active" && eligible && pending.SessionID == session.ID && pending.SessionSeen != nil && pending.MessageSeen == nil
}

func validateStreamSessionHistory(states map[string]streamSessionSeen, event model.EventSession, guard streamGuard) error {
	if event.Previous == "" {
		return nil
	}
	previous, known := states[event.ID]
	if !known || previous.Guard != guard || previous.State != event.Previous || previous.Retired != event.PreviousRetired {
		return statefile.ErrInvalidFile
	}
	return nil
}

func validateStreamMessageReceipt(state pluginStreamState, pending *streamPending, wire model.EventMessage) error {
	if !matchingStreamMessageReceipt(pending, wire) {
		return statefile.ErrInvalidFile
	}
	proof := pending.MessageSeen
	primary := map[string]streamDependency{}
	visible := 0
	endpoints := []struct{ actual, shown string }{{proof.From, wire.From}, {proof.To, wire.To}}
	for _, endpoint := range endpoints {
		seen, err := validateStreamMessageEndpoint(state.Installation, endpoint.actual, endpoint.shown, pending.Guards)
		if err != nil {
			return err
		}
		if !model.IsPluginInboxAddress(endpoint.actual) {
			primary[endpoint.actual] = pending.Guards[endpoint.actual]
		}
		if seen {
			visible++
		}
	}
	if visible == 0 || !maps.Equal(primary, proof.Guards) {
		return statefile.ErrInvalidFile
	}
	return validateStreamMessageHistory(state.Seen, pending, wire, primary)
}

func matchingStreamMessageReceipt(pending *streamPending, wire model.EventMessage) bool {
	proof := pending.MessageSeen
	return pending.Retirement == nil && proof != nil && pending.SessionSeen == nil && pending.SessionID == "" && len(pending.Guards) <= 4 && proof.ID == wire.ID && proof.Position == pending.After && proof.Status == wire.Status && proof.Held == wire.Held && proof.Acknowledged == wire.Acknowledged
}

func validateStreamMessageEndpoint(installation, actual, shown string, guards map[string]streamDependency) (bool, error) {
	if model.ValidateSessionAddress(actual) != nil || (shown != actual && shown != addressKind(actual)) {
		return false, statefile.ErrInvalidFile
	}
	if model.IsPluginInboxAddress(actual) {
		if shown == actual && actual != "plugin:"+installation {
			return false, statefile.ErrInvalidFile
		}
		return false, nil
	}
	dependency, exists := guards[actual]
	if !exists || (shown == actual && dependency.Class != "active") {
		return false, statefile.ErrInvalidFile
	}
	return shown == actual, nil
}

func validateStreamMessageHistory(seen []streamMessageSeen, pending *streamPending, wire model.EventMessage, primary map[string]streamDependency) error {
	proof := pending.MessageSeen
	if wire.Previous != "" {
		previous := streamSeenMessage(seen, wire.ID)
		if previous == nil || previous.From != proof.From || previous.To != proof.To || !maps.Equal(previous.Guards, primary) || previous.Status != wire.Previous || previous.Held != wire.PreviousHeld || previous.Acknowledged != wire.PreviousAcknowledged {
			return statefile.ErrInvalidFile
		}
	}
	if wire.ReplyTo != "" {
		parent := streamSeenMessage(seen, wire.ReplyTo)
		if parent == nil || !streamDependenciesMatch(parent.Guards, pending.Guards) {
			return statefile.ErrInvalidFile
		}
	}
	return nil
}
