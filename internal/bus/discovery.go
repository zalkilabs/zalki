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
	State        model.SessionState   `json:"state,omitempty"`
	Liveness     model.Liveness       `json:"liveness,omitempty"`
	UpdatedAt    string               `json:"updated_at"`
	Delivery     model.DeliveryTiming `json:"delivery"`
	IdleWake     *bool                `json:"idle_wake,omitempty"`
	Experimental bool                 `json:"experimental,omitempty"`
	// ApprovalSince is when the oldest message held for this session's permission
	// prompt started to wait. It is set only while a message is held.
	ApprovalSince string `json:"approval_since,omitempty"`
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
	var sessions []model.Session
	var skipped int
	approvalSince := map[string]string{}
	err := s.readLocked(func() error {
		if actor != "" {
			if _, err := s.authenticate(actor, key); err != nil {
				return err
			}
		}
		var err error
		sessions, skipped, err = s.sessionsWithWarnings()
		if err != nil {
			return err
		}
		collectHoldStarts(s, sessions, approvalSince)
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	info := make([]AgentInfo, 0, len(sessions))
	for _, v := range sessions {
		row := AgentInfo{ID: v.ID, Name: v.Name, Self: v.ID == actor, Task: v.Task, CWD: v.CWD, State: v.State,
			Liveness: v.Liveness, UpdatedAt: v.UpdatedAt,
			Delivery: v.Receive.BusyDelivery, Experimental: v.Receive.Experimental}
		row = withApprovalView(row, v, approvalSince[v.ID])
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

// oldestHold is a plain function, so the Store gains no method. It is the time the oldest message held for target's permission prompt
// started to wait, or "" when no readable held message has one. The caller holds
// the journal lock. It reads, and changes nothing.
func oldestHold(s Store, target string) string {
	queued, _, err := approvals(s).queuedApprovals(target)
	if err != nil {
		return ""
	}
	oldest, oldestAt := "", time.Time{}
	for _, msg := range queued {
		if msg.Receipt == nil {
			continue
		}
		// Compare the instants, not the text: ".1Z" is earlier than ".11Z".
		if at, err := time.Parse(time.RFC3339Nano, msg.Receipt.At); err == nil && (oldest == "" || at.Before(oldestAt)) {
			oldest, oldestAt = msg.Receipt.At, at
		}
	}
	return oldest
}

// collectHoldStarts fills since with the oldest hold start of each session that
// has held messages. Only such a session causes a message scan.
func collectHoldStarts(s Store, sessions []model.Session, since map[string]string) {
	for _, v := range sessions {
		if v.ApprovalHeld > 0 {
			since[v.ID] = oldestHold(s, v.ID)
		}
	}
}

// withApprovalView shows a session with a held message as waiting for a
// permission prompt, also for a client whose hook does not say so. It changes
// the view only: no stored state changes.
func withApprovalView(row AgentInfo, v model.Session, since string) AgentInfo {
	if since != "" && !v.Retired {
		row.State, row.ApprovalSince = model.StateApproval, since
	}
	return row
}
