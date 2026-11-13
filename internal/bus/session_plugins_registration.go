package bus

import (
	"bytes"
	"encoding/json"
	"log/slog"

	"github.com/zalkilabs/zalki/internal/model"
)

// Reconcile only after loading the current record under the publication lock.
func retainSessionPluginPolicy(s Store, current model.Session, next *model.Session, compact, exists bool) error {
	if !exists {
		return nil
	}
	if compact && !current.Retired {
		next.PluginPolicy = bytes.Clone(current.PluginPolicy)
		return nil
	}
	if len(current.PluginPolicy) == 0 {
		return nil
	}
	old, valid := decodeSessionPluginPolicy(current)
	fresh, initialized := decodeSessionPluginPolicy(*next)
	if !initialized {
		return model.NewError(model.CodeUnavailable, pluginPolicyUnavailable)
	}
	if !valid {
		if fresh.Mode == "off" && !fresh.InvalidEnv {
			s.logEvent(slog.LevelWarn, "session.plugin_policy_repair_requested", eventData{Actor: next.ID, Agent: next.Kind})
			return nil
		}
		return model.NewError(model.CodeUnavailable, pluginPolicyUnavailable)
	}
	fresh.Muted, fresh.LastChange = old.Muted, old.LastChange
	raw, err := encodeSessionPluginPolicy(fresh)
	if err == nil {
		next.PluginPolicy = raw
	}
	return err
}

// Uses only the preimage already read by saveSessionWith: no additional I/O,
// probe or Store lock. Recovery restores prepared bytes and never calls this.
func refreshSessionPluginAudit(prior []byte, next *model.Session) error {
	var before model.Session
	if len(prior) == 0 {
		return nil
	}
	if err := json.Unmarshal(prior, &before); err != nil {
		return err
	}
	if before.Generation != next.Generation {
		return nil
	}
	old, valid := decodeSessionPluginPolicy(before)
	if !valid {
		next.PluginPolicy = bytes.Clone(before.PluginPolicy)
		return nil
	}
	if before.Retired == next.Retired && samePluginOwner(before.Owner, next.Owner) {
		return nil
	}
	p, valid := decodeSessionPluginPolicy(*next)
	if !valid || p.LastChange.ID != old.LastChange.ID {
		return nil
	}
	p.LastChange = sessionPluginAudit(next.ID)
	raw, err := encodeSessionPluginPolicy(p)
	if err == nil {
		next.PluginPolicy = raw
	}
	return err
}

func samePluginOwner(a, b *model.ProcessRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
