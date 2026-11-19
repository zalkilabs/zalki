package pluginfeed

import "context"

// streamStatus only reads the bounded atomic checkpoint. Owner discovery and
// update snapshots may call it without Store entry, recovery, state creation,
// native enumeration or an attempt/cursor change.
func (s pluginFiles) streamStatus(owner pluginRecord) (bool, string) {
	if owner.ID == "" {
		return false, ""
	}
	state, exists, err := s.loadStreamState(newStreamBudget(context.Background()))
	if err != nil {
		return owner.Enabled && owner.Access.Status, "unavailable"
	}
	if !exists || state.Installation != owner.ID || state.Enable != owner.Epoch || state.Revision != owner.Revision {
		if owner.Enabled && owner.Access.Status {
			return true, "enabled"
		}
		return false, ""
	}
	pending := owner.Enabled && owner.Access.Status && state.Pending != nil && state.Pending.Kind == "gap"
	return pending, state.LastGap
}
