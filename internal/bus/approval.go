package bus

import (
	"context"
	"errors"
	"os"
	"sort"
)

// approvalQueueCap bounds messages waiting on one session's permission prompt.
const approvalQueueCap = 8

// saveReservation writes the count before a held reservation so a crash cannot
// leave a durable held message with a zero count. The caller holds the lock.
func (s Store) saveReservation(msg Message) error {
	held := msg.IsQueuedForApproval()
	if held {
		if err := s.rememberApprovalHold(msg.To, 1); err != nil {
			return err
		}
	}
	if err := s.saveMessage(msg); err != nil {
		if held {
			_ = s.rememberApprovalHold(msg.To, -1)
		}
		return err
	}
	return nil
}

// holdForApproval marks msg as waiting on the target's permission prompt.
// The caller holds the journal lock, then uses saveReservation to persist the
// matching held count and message.
func (s Store) holdForApproval(msg *Message) error {
	target, err := s.session(msg.To)
	if err != nil {
		return err
	}
	if target.ApprovalHeld >= approvalQueueCap {
		return fail(CodeUnavailable, "too many messages are waiting for this session's permission prompt")
	}
	return msg.hold()
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
	queued := []Message{}
	if err := s.eachMessage(func(_ string, msg Message, readErr error) error {
		if readErr != nil {
			if skipUnreadable {
				return nil
			}
			return readErr
		}
		if msg.To == target && msg.IsQueuedForApproval() {
			queued = append(queued, msg)
		}
		return nil
	}); err != nil {
		return nil, err
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
	if err := current.settleNotSent(CodeUnavailable, reason); err != nil {
		return err
	}
	return s.saveMessage(current)
}

// approvalHolds counts messages this build queued, one pass over the journal.
// The caller holds the journal lock. Legacy approval refusals are not counted.
// Unreadable files are returned by id so prune can report them and continue.
func (s Store) approvalHolds() (map[string]int, []string, error) {
	counts := map[string]int{}
	unreadable := []string{}
	if err := s.eachMessage(func(id string, msg Message, readErr error) error {
		if readErr != nil {
			unreadable = append(unreadable, id)
			return nil
		}
		if msg.IsQueuedForApproval() {
			counts[msg.To]++
		}
		return nil
	}); err != nil {
		return nil, nil, err
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

type approvalResolution struct {
	jobs        []Message
	unreachable bool
}

// resolveApprovalQueue owns settlement/adoption under the journal lock.
// A hook poll preserves an outstanding approval count until the prompt resolves;
// lifecycle release also repairs an empty count while approval is still open.
func (s Store) resolveApprovalQueue(current Session, hookPoll bool) (approvalResolution, error) {
	reason := ""
	if current.Retired {
		reason = "destination has retired"
	} else if s.sessionLiveness(current) == livenessExited {
		reason = "destination owner process has exited"
	}
	result := approvalResolution{unreachable: reason != ""}
	if current.ApprovalHeld == 0 || (hookPoll && reason == "" && current.State == StateApproval) {
		return result, nil
	}
	queued, err := s.queuedApprovals(current.ID)
	if err != nil {
		return result, err
	}
	if len(queued) == 0 {
		return result, s.clearApprovalHeld(current.ID)
	}
	if reason != "" {
		for _, msg := range queued {
			if err := s.settleQueued(msg, reason); err != nil {
				return result, err
			}
		}
	} else {
		if current.State == StateApproval {
			return result, nil
		}
		result.jobs, err = s.releaseReadyApprovals(current, queued)
		if err != nil {
			return result, err
		}
	}
	return result, s.clearApprovalHeld(current.ID)
}

func (s Store) releaseReadyApprovals(current Session, queued []Message) ([]Message, error) {
	var jobs []Message
	next := StatusAttempting
	if current.Receive.HookDelivery {
		next = StatusPendingActivity
	}
	for _, msg := range queued {
		if msg.TargetGeneration != current.Generation {
			if err := s.settleQueued(msg, "destination registration changed before delivery"); err != nil {
				return nil, err
			}
			continue
		}
		if err := msg.releaseHeld(next); err != nil {
			return nil, err
		}
		if err := s.saveMessage(msg); err != nil {
			return nil, err
		}
		if !current.Receive.HookDelivery {
			jobs = append(jobs, msg)
		}
	}
	return jobs, nil
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
		target = current
		resolved, err := s.Store.resolveApprovalQueue(current, false)
		jobs = resolved.jobs
		return err
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
