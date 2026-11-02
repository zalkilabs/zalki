package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// approvalQueueCap bounds messages waiting on one session's permission prompt.
const approvalQueueCap = 8

const approvalQueuedNote = "queued until the permission prompt is answered"

// IsQueuedForApproval distinguishes a held message from a legacy final refusal.
// The derived queued_for_approval view is never written to the journal.
func (msg Message) IsQueuedForApproval() bool {
	return msg.Held && msg.Status == StatusApprovalBlocked
}

// holdForApproval marks msg as waiting on the target's permission prompt.
// The caller holds the journal lock and then saves the message with
// rememberApprovalHold so the session count matches the journal.
func (s Store) holdForApproval(msg *Message) error {
	target, err := s.session(msg.To)
	if err != nil {
		return err
	}
	if target.ApprovalHeld >= approvalQueueCap {
		return fail(CodeUnavailable, "too many messages are waiting for this session's permission prompt")
	}
	msg.Status = StatusApprovalBlocked
	msg.Held = true
	msg.Receipt = &Receipt{At: timestamp(), Status: StatusApprovalBlocked, Error: approvalQueuedNote}
	return nil
}

// rememberApprovalHold moves the target's queued-message count by delta.
// The caller holds the journal lock. A positive delta is written before the
// message so a crash cannot leave a queued message with a zero count.
func (s Store) rememberApprovalHold(targetID string, delta int) error {
	if delta == 0 {
		return nil
	}
	current, err := s.session(targetID)
	if err != nil {
		return err
	}
	current.ApprovalHeld += delta
	if current.ApprovalHeld < 0 {
		current.ApprovalHeld = 0
	}
	return s.saveSession(current)
}

func (s Store) clearApprovalHeld(targetID string) error {
	current, err := s.session(targetID)
	if err != nil {
		return err
	}
	if current.ApprovalHeld == 0 {
		return nil
	}
	current.ApprovalHeld = 0
	return s.saveSession(current)
}

// queuedApprovals returns approval-blocked messages for target in send order.
// The caller holds the journal lock.
func (s Store) queuedApprovals(target string) ([]Message, error) {
	return s.queuedApprovalsRead(target, false)
}

func (s Store) queuedApprovalsRead(target string, skipUnreadable bool) ([]Message, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "messages", "*.json"))
	if err != nil {
		return nil, err
	}
	queued := []Message{}
	for _, file := range files {
		msg, err := s.message(strings.TrimSuffix(filepath.Base(file), ".json"))
		if err != nil {
			if skipUnreadable {
				continue
			}
			return nil, err
		}
		if msg.To == target && msg.IsQueuedForApproval() {
			queued = append(queued, msg)
		}
	}
	sort.Slice(queued, func(i, j int) bool {
		if queued[i].CreatedAt == queued[j].CreatedAt {
			return queued[i].ID < queued[j].ID
		}
		return queued[i].CreatedAt < queued[j].CreatedAt
	})
	return queued, nil
}

func (s Store) settleQueued(msg Message, reason string) error {
	current, err := s.message(msg.ID)
	if err != nil {
		return err
	}
	if !current.IsQueuedForApproval() {
		return nil
	}
	current.Held = false
	current.Status = StatusNotSent
	current.Receipt = &Receipt{At: timestamp(), Status: StatusNotSent, Code: CodeUnavailable, Error: reason}
	return s.saveMessage(current)
}

// approvalHolds counts messages this build queued, one pass over the journal.
// The caller holds the journal lock. Legacy approval refusals are not counted.
// Unreadable files are returned by id so prune can report them and continue.
func (s Store) approvalHolds() (map[string]int, []string, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "messages", "*.json"))
	if err != nil {
		return nil, nil, err
	}
	counts := map[string]int{}
	unreadable := []string{}
	for _, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".json")
		msg, err := s.message(id)
		if err != nil {
			unreadable = append(unreadable, id)
			continue
		}
		if msg.IsQueuedForApproval() {
			counts[msg.To]++
		}
	}
	sort.Strings(unreadable)
	return counts, unreadable, nil
}

// reconcileApprovalHolds repairs a count an older build dropped when it rewrote
// a session. Retired targets are settled, because no later hook will release them.
// The caller holds the journal lock.
func (s Store) reconcileApprovalHolds(dryRun bool, report *PruneReport) error {
	counts, unreadable, err := s.approvalHolds()
	if err != nil {
		return err
	}
	for _, id := range unreadable {
		report.Sessions = append(report.Sessions, PruneItem{ID: id, Reason: "message_unreadable"})
	}
	retiring := map[string]bool{}
	for _, item := range report.Sessions {
		if item.Reason == "explicit_cleanup" || item.Reason == "owner_exited" {
			retiring[item.ID] = true
		}
	}
	sessions, err := s.rawSessions()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, v := range sessions {
		seen[v.ID] = true
		count := counts[v.ID]
		if v.Retired || retiring[v.ID] {
			if count == 0 && v.ApprovalHeld == 0 {
				continue
			}
			reason := "approval_recounted"
			if count > 0 {
				reason = "approval_settled"
			}
			report.Sessions = append(report.Sessions, PruneItem{ID: v.ID, Reason: reason})
			if dryRun {
				continue
			}
			if count > 0 {
				if err := s.settleReadableApprovalQueue(v.ID, "destination has retired"); err != nil {
					return err
				}
			}
			if err := s.clearApprovalHeld(v.ID); err != nil {
				return err
			}
			continue
		}
		if v.ApprovalHeld == count {
			continue
		}
		report.Sessions = append(report.Sessions, PruneItem{ID: v.ID, Reason: "approval_recounted"})
		if dryRun {
			continue
		}
		v.ApprovalHeld = count
		if err := s.saveSession(v); err != nil {
			return err
		}
	}
	missing := []string{}
	for id, count := range counts {
		if !seen[id] && count > 0 {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	for _, id := range missing {
		report.Sessions = append(report.Sessions, PruneItem{ID: id, Reason: "approval_settled"})
		if dryRun {
			continue
		}
		if err := s.settleReadableApprovalQueue(id, "destination has retired"); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) settleApprovalQueue(actor, reason string) error {
	return s.settleApprovalQueueRead(actor, reason, false)
}

// settleReadableApprovalQueue settles queued messages and ignores files prune
// has already reported as unreadable, so one bad file cannot abort cleanup.
func (s Store) settleReadableApprovalQueue(actor, reason string) error {
	return s.settleApprovalQueueRead(actor, reason, true)
}

func (s Store) settleApprovalQueueRead(actor, reason string, skipUnreadable bool) error {
	queued, err := s.queuedApprovalsRead(actor, skipUnreadable)
	if err != nil {
		return err
	}
	for _, msg := range queued {
		if err := s.settleQueued(msg, reason); err != nil {
			return err
		}
	}
	return nil
}

// adoptApprovalQueue moves a resolved session's queued messages into the hook
// inbox. A later EmitPending writes them into hook context. The caller holds
// the journal lock.
func (s Store) adoptApprovalQueue(current Session) error {
	queued, err := s.queuedApprovals(current.ID)
	if err != nil {
		return err
	}
	for _, msg := range queued {
		if msg.TargetGeneration != current.Generation {
			if err := s.settleQueued(msg, "destination registration changed before delivery"); err != nil {
				return err
			}
			continue
		}
		msg.Held = false
		msg.Status = StatusPendingActivity
		msg.Receipt = &Receipt{At: timestamp(), Status: StatusPendingActivity}
		if err := s.saveMessage(msg); err != nil {
			return err
		}
	}
	return nil
}

// releaseApprovalQueue delivers messages that were waiting for actor's
// permission prompt. It runs after the hook has recorded the new session
// state. Retired and exited sessions end those messages as not sent.
func (s *Service) releaseApprovalQueue(ctx context.Context, actor string) error {
	var jobs []Message
	var target Session
	err := s.Store.locked(func() error {
		current, err := s.Store.session(actor)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.ApprovalHeld == 0 {
			return nil
		}
		queued, err := s.Store.queuedApprovals(actor)
		if err != nil {
			return err
		}
		if len(queued) == 0 {
			return s.Store.clearApprovalHeld(actor)
		}
		if current.Retired {
			if err := s.Store.settleApprovalQueue(actor, "destination has retired"); err != nil {
				return err
			}
			return s.Store.clearApprovalHeld(actor)
		}
		if s.Store.sessionLiveness(current) == livenessExited {
			if err := s.Store.settleApprovalQueue(actor, "destination owner process has exited"); err != nil {
				return err
			}
			return s.Store.clearApprovalHeld(actor)
		}
		if current.State == StateApproval {
			return nil
		}
		if current.Receive.HookDelivery {
			if err := s.Store.adoptApprovalQueue(current); err != nil {
				return err
			}
			return s.Store.clearApprovalHeld(actor)
		}
		target = current
		for _, msg := range queued {
			if msg.TargetGeneration != current.Generation {
				if err := s.Store.settleQueued(msg, "destination registration changed before delivery"); err != nil {
					return err
				}
				continue
			}
			currentMsg, err := s.Store.message(msg.ID)
			if err != nil {
				return err
			}
			if !currentMsg.IsQueuedForApproval() {
				continue
			}
			currentMsg.Held = false
			currentMsg.Status = StatusAttempting
			currentMsg.Receipt = nil
			if err := s.Store.saveMessage(currentMsg); err != nil {
				return err
			}
			jobs = append(jobs, currentMsg)
		}
		return s.Store.clearApprovalHeld(actor)
	})
	if err != nil || len(jobs) == 0 {
		return err
	}
	var first error
	for _, msg := range jobs {
		if _, err := s.deliverReserved(ctx, msg, target); err != nil && first == nil {
			first = err
		}
	}
	return first
}
