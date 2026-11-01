package bus

import "time"

// AgentInfo is the discovery view. Journal schemas, native IDs, adapter state,
// credential hashes and endpoint details are not part of the MCP contract.
type AgentInfo struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// Task is the agent's own unverified claim. Empty claims are omitted.
	Task         string         `json:"task,omitempty"`
	CWD          string         `json:"cwd"`
	State        SessionState   `json:"state,omitempty"`
	Liveness     string         `json:"liveness,omitempty"`
	UpdatedAt    string         `json:"updated_at"`
	Delivery     DeliveryTiming `json:"delivery"`
	IdleWake     *bool          `json:"idle_wake,omitempty"`
	Experimental bool           `json:"experimental,omitempty"`
}

func (s Store) Discover() ([]AgentInfo, error) {
	sessions, err := s.List()
	if err != nil {
		return nil, err
	}
	info := make([]AgentInfo, 0, len(sessions))
	for _, v := range sessions {
		row := AgentInfo{ID: v.ID, Name: v.Name, Task: v.Task, CWD: v.CWD, State: v.State,
			Liveness: v.Liveness, UpdatedAt: v.UpdatedAt,
			Delivery: v.Receive.BusyDelivery, Experimental: v.Receive.Experimental}
		if row.State == StateReady {
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
		if v.Receive.IdleWake != (row.Delivery != DeliveryNextHook) {
			idleWake := v.Receive.IdleWake
			row.IdleWake = &idleWake
		}
		info = append(info, row)
	}
	return info, nil
}
