package pluginfeed

import (
	"context"
	"encoding/json"
	"maps"
	"slices"

	"github.com/zalkilabs/zalki/internal/model"
)

func streamEventTargets(event model.Event, state pluginStreamState) []string {
	ids := []string{}
	if event.Message == nil {
		return []string{event.Session.ID}
	}
	for _, id := range []string{event.Message.From, event.Message.To} {
		if !model.IsPluginInboxAddress(id) {
			ids = append(ids, id)
		}
	}
	if parent := streamSeenMessage(state.Seen, event.Message.ReplyTo); parent != nil {
		for id := range parent.Guards {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}

func streamSeenMessage(seen []streamMessageSeen, id string) *streamMessageSeen {
	for i := len(seen) - 1; i >= 0; i-- {
		if seen[i].ID == id {
			return &seen[i]
		}
	}
	return nil
}

func (s PluginFeed) scanStream(ctx context.Context, step *streamStep) error {
	state := &step.state
	if state.Cursor.Epoch == "" && step.head.Epoch != "" {
		state.Cursor.Epoch = step.head.Epoch
	}
	for scanned, input := 0, 0; state.Cursor.Sequence < step.head.Last && scanned < 64 && input < streamBatchLimit; scanned++ {
		sequence := state.Cursor.Sequence + 1
		event, err := step.budget.eventAt(step.view, step.head.Epoch, sequence)
		if err != nil {
			s.noteStreamReadFailure(event.ID, err)
			return err
		}
		// Count the exact metadata encoding too; native EventAt separately
		// charges its persisted input before allocation, with the shared budget.
		raw, err := json.Marshal(event)
		if err != nil {
			return err
		}
		input += len(raw)
		pending, reset, err := s.projectStreamEvent(ctx, step, event, pluginPosition{Epoch: step.head.Epoch, Sequence: sequence})
		if err != nil {
			s.noteStreamIntegrity(event.ID, "membership_unavailable", err)
			return err
		}
		if reset {
			return state.queueGap("policy", streamAt(step.head))
		}
		if pending != nil {
			state.Pending = pending
			return nil
		}
		state.Cursor.Sequence = sequence
	}
	return nil
}

func (s PluginFeed) projectStreamEvent(ctx context.Context, step *streamStep, event model.Event, position pluginPosition) (*streamPending, bool, error) {
	collection, err := collectStreamMembers(ctx, step.view, step.owner, streamEventTargets(event, step.state), step.budget)
	if err != nil {
		return nil, false, err
	}
	projection, reset := refreshStreamEventRows(&step.state, collection.Records, event, position, streamAt(step.head))
	if reset {
		return nil, true, nil
	}
	current, dependencies, retirement := projection.rows, projection.dependencies, projection.retirement
	allowed := func(id string) bool {
		return streamEventAllowed(step.state, current, position, event.Message != nil, id)
	}
	pending := &streamPending{ID: event.ID, Kind: "event", After: position, Guards: dependencies}
	if event.Message == nil {
		guard := current[event.Session.ID].Guard
		if retirement != nil {
			guard = retirement.Before
			pending.Retirement, pending.ResetAfter = retirement, "membership"
		}
		if retirement == nil && !streamSessionEventEligible(event.Session, guard, allowed(event.Session.ID)) {
			return nil, false, nil
		}
		projectStreamSession(&step.state, &event, pending, guard)
	} else if !projectStreamMessage(&step.state, step.owner, &event, pending, dependencies, allowed) {
		return nil, false, nil
	}
	raw, err := encodeStreamFrame(streamFrame{ID: pending.ID, Event: &event})
	if err != nil {
		return nil, false, err
	}
	pending.Frames = []json.RawMessage{raw}
	return pending, false, pending.validate()
}

type streamEventView struct {
	rows         map[string]streamRow
	dependencies map[string]streamDependency
	retirement   *streamRetirement
}

func refreshStreamEventRows(state *pluginStreamState, rows []streamRow, event model.Event, position, head pluginPosition) (streamEventView, bool) {
	view := streamEventView{rows: map[string]streamRow{}, dependencies: map[string]streamDependency{}}
	for _, row := range rows {
		if event.Message == nil && row.ID == event.Session.ID {
			view.retirement = streamRetirementCandidate(*state, event, row, position)
		}
		view.rows[row.ID] = row
		dependency := streamDependency{Guard: row.Guard, Class: row.Class}
		if view.retirement != nil && row.ID == event.Session.ID {
			dependency = streamDependency{Guard: view.retirement.Before, Class: "active"}
		}
		view.dependencies[row.ID] = dependency
		if old, ok := state.Rows[row.ID]; ok && (old.Guard != row.Guard || old.Class != row.Class) && view.retirement == nil {
			return view, true
		}
		forgetChangedStreamProofs(state, row.ID, dependency)
		state.rememberStreamRow(row, head)
	}
	return view, false
}

func forgetChangedStreamProofs(state *pluginStreamState, id string, dependency streamDependency) {
	state.Seen = slices.DeleteFunc(state.Seen, func(seen streamMessageSeen) bool {
		original, depends := seen.Guards[id]
		return depends && original != dependency
	})
}

func streamEventAllowed(state pluginStreamState, current map[string]streamRow, position pluginPosition, message bool, id string) bool {
	row, known := state.Rows[id]
	fresh, exists := current[id]
	if !known || !exists || fresh.Class != "active" || row.Guard != fresh.Guard {
		return false
	}
	floor := row.Floor
	if message {
		if row.Since == nil {
			return false
		}
		floor = *row.Since
	}
	return streamPositionAfter(position, floor)
}

func streamSessionEventEligible(event model.EventSession, guard streamGuard, allowed bool) bool {
	return allowed && !event.Retired && event.Cause != model.CauseOwnerExited && event.Generation == guard.Generation
}

func projectStreamSession(state *pluginStreamState, event *model.Event, pending *streamPending, guard streamGuard) {
	previous, known := state.States[event.Session.ID]
	if !known || previous.Guard != guard || previous.State != event.Session.Previous || previous.Retired != event.Session.PreviousRetired {
		event.Session.Previous, event.Session.PreviousRetired = "", false
	}
	pending.SessionID = event.Session.ID
	pending.SessionSeen = &streamSessionSeen{Guard: guard, State: event.Session.State, Retired: event.Session.Retired}
}

func projectStreamMessage(state *pluginStreamState, owner pluginRecord, event *model.Event, pending *streamPending, dependencies map[string]streamDependency, allowed func(string) bool) bool {
	m := *event.Message
	from, to := allowed(m.From), allowed(m.To)
	if !from && !to {
		return false
	}
	actualFrom, actualTo := m.From, m.To
	primary := map[string]streamDependency{}
	for _, id := range []string{actualFrom, actualTo} {
		if dependency, ok := dependencies[id]; ok {
			primary[id] = dependency
		}
	}
	previous := streamSeenMessage(state.Seen, m.ID)
	if !streamMessagePreviousKnown(previous, m, primary) {
		m.Previous, m.PreviousHeld, m.PreviousAcknowledged = "", false, false
	} else {
		state.touchStreamProof(m.ID)
	}
	parent := streamSeenMessage(state.Seen, m.ReplyTo)
	if parent == nil || !streamDependenciesMatch(parent.Guards, dependencies) {
		m.ReplyTo = ""
	} else {
		state.touchStreamProof(m.ReplyTo)
	}
	if !from && actualFrom != "plugin:"+owner.ID {
		m.From = addressKind(actualFrom)
	}
	if !to && actualTo != "plugin:"+owner.ID {
		m.To = addressKind(actualTo)
	}
	event.Message = &m
	pending.MessageSeen = &streamMessageSeen{ID: m.ID, From: actualFrom, To: actualTo, Guards: primary, Position: pending.After, Status: m.Status, Held: m.Held, Acknowledged: m.Acknowledged}
	return true
}

func streamMessagePreviousKnown(previous *streamMessageSeen, message model.EventMessage, primary map[string]streamDependency) bool {
	return previous != nil && previous.From == message.From && previous.To == message.To && maps.Equal(previous.Guards, primary) && previous.Status == message.Previous && previous.Held == message.PreviousHeld && previous.Acknowledged == message.PreviousAcknowledged
}

func (state *pluginStreamState) touchStreamProof(id string) {
	proof := streamSeenMessage(state.Seen, id)
	if proof == nil {
		return
	}
	proofCopy := *proof
	state.Seen = slices.DeleteFunc(state.Seen, func(entry streamMessageSeen) bool { return entry.ID == id })
	state.Seen = append(state.Seen, proofCopy)
}

func streamDependenciesMatch(original, current map[string]streamDependency) bool {
	if len(original) == 0 {
		return false
	}
	for id, dependency := range original {
		if now, ok := current[id]; !ok || now != dependency {
			return false
		}
	}
	return true
}

func (state *pluginStreamState) rememberStreamRow(row streamRow, floor pluginPosition) {
	if row.Class == "absent" {
		return
	}
	if _, known := state.Rows[row.ID]; !known {
		state.purgeStreamSession(row.ID)
		row.Floor = floor
		row.Since = &floor
		state.Rows[row.ID] = row
	}
	state.RowOrder = slices.DeleteFunc(state.RowOrder, func(id string) bool { return id == row.ID })
	state.RowOrder = append(state.RowOrder, row.ID)
	for len(state.RowOrder) > 64 {
		id := state.RowOrder[0]
		state.RowOrder = state.RowOrder[1:]
		delete(state.Rows, id)
		state.purgeStreamSession(id)
	}
}

func streamPositionAfter(position, floor pluginPosition) bool {
	return (floor.Epoch == "" && floor.Sequence == 0) || (position.Epoch == floor.Epoch && position.Sequence > floor.Sequence)
}

func (state *pluginStreamState) purgeStreamSession(id string) {
	delete(state.States, id)
	state.Seen = slices.DeleteFunc(state.Seen, func(seen streamMessageSeen) bool {
		_, depends := seen.Guards[id]
		return depends
	})
}
