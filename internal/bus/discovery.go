package bus

import (
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// AgentInfo is the discovery view. Journal schemas, native IDs, adapter state,
// credential hashes and endpoint details are not part of the MCP contract.
type AgentInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Self bool   `json:"self,omitempty"`
	// Task is the agent's own unverified claim. Empty claims are omitted.
	Task         string               `json:"task,omitempty"`
	CWD          string               `json:"cwd"`
	State        SessionState         `json:"state,omitempty"`
	Liveness     model.Liveness       `json:"liveness,omitempty"`
	UpdatedAt    string               `json:"updated_at"`
	Delivery     model.DeliveryTiming `json:"delivery"`
	IdleWake     *bool                `json:"idle_wake,omitempty"`
	Experimental bool                 `json:"experimental,omitempty"`
}

func (s Store) Discover() ([]AgentInfo, error) {
	return s.DiscoverFor("", "")
}

// DiscoverFor optionally marks the authenticated caller. Partial or invalid
// credentials fail the entire call rather than silently losing self identity.
func (s Store) DiscoverFor(actor, key string) ([]AgentInfo, error) {
	info, _, err := s.DiscoverWithWarnings(actor, key)
	return info, err
}

func (s Store) DiscoverWithWarnings(actor, key string) ([]AgentInfo, int, error) {
	if (actor == "") != (key == "") {
		return nil, 0, model.NewError(model.CodeInvalidInput, "actor and capability must be provided together")
	}
	var sessions []Session
	var skipped int
	err := s.readLocked(func() error {
		if actor != "" {
			if _, err := s.authenticate(actor, key); err != nil {
				return err
			}
		}
		var err error
		sessions, skipped, err = s.sessionsWithWarnings()
		return err
	})
	if err != nil {
		return nil, 0, err
	}
	info := make([]AgentInfo, 0, len(sessions))
	for _, v := range sessions {
		row := AgentInfo{ID: v.ID, Name: v.Name, Self: v.ID == actor, Task: v.Task, CWD: v.CWD, State: v.State,
			Liveness: v.Liveness, UpdatedAt: v.UpdatedAt,
			Delivery: v.Receive.BusyDelivery, Experimental: v.Receive.Experimental}
		if row.State == model.StateReady {
			row.State = ""
		}
		if row.Liveness == "unknown" {
			row.Liveness = ""
		}
		if updated, err := time.Parse(time.RFC3339Nano, row.UpdatedAt); err == nil {
			row.UpdatedAt = updated.UTC().Format(time.RFC3339)
		}
		// Built-ins wake idle sessions except when delivery requires a hook.
		// An explicit override preserves other combinations for external adapters.
		if v.Receive.IdleWake != (row.Delivery != model.DeliveryNextHook) {
			idleWake := v.Receive.IdleWake
			row.IdleWake = &idleWake
		}
		info = append(info, row)
	}
	return info, skipped, nil
}
