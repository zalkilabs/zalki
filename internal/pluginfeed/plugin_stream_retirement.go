package pluginfeed

import (
	"github.com/zalkilabs/zalki/internal/model"
)

// Private source-attested same-write proof, never serialized into a child frame.
type streamRetirement struct {
	Event        string      `json:"event"`
	Before       streamGuard `json:"before"`
	After        streamGuard `json:"after"`
	Digest       string      `json:"digest"`
	BeforeDigest string      `json:"before_digest"`
}

func validStreamRetirement(proof *streamRetirement, event model.Event) bool {
	if event.Session.Cause == model.CauseOwnerExited || proof == nil || event.Type != model.EventSessionStateChanged || !event.Session.Retired || event.Session.PreviousRetired || proof.Event != event.ID || !validPluginSHA(proof.Digest) || !validPluginSHA(proof.BeforeDigest) {
		return false
	}
	before, after := proof.Before, proof.After
	if before.Generation != after.Generation {
		return false
	}
	return after.Generation == event.Session.Generation && before.Owner == after.Owner && validPluginSHA(before.Owner) && model.IsCanonicalUUID(before.Audit) && model.IsCanonicalUUID(after.Audit) && before.Audit != after.Audit
}

func streamRetirementCandidate(state pluginStreamState, event model.Event, row streamRow, position pluginPosition) *streamRetirement {
	if !validStreamRetirement(row.Retirement, event) || row.Class != "absent" || row.Guard != row.Retirement.After || row.Digest != row.Retirement.Digest {
		return nil
	}
	previous, exposed := state.States[event.Session.ID]
	known, included := state.Rows[event.Session.ID]
	if !exposed || !included || previous.Retired || previous.Guard != row.Retirement.Before || known.Class != "active" || known.Guard != row.Retirement.Before || !streamPositionAfter(position, known.Floor) {
		return nil
	}
	proofCopy := *row.Retirement
	return &proofCopy
}

func streamRetirementCurrent(pending *streamPending, row streamRow) bool {
	return pending.Retirement != nil && row.Retirement != nil && *pending.Retirement == *row.Retirement && row.Class == "absent" && row.Guard == row.Retirement.After && row.Digest == row.Retirement.Digest
}
