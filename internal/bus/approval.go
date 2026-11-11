package bus

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"

	"github.com/zalkilabs/zalki/internal/model"
)

// approvalQueueCap bounds messages waiting on one session's permission prompt.
const approvalQueueCap = 8

// saveReservation writes the count before a held reservation so a crash cannot
// leave a durable held message with a zero count. The caller holds the lock.
func (s Store) saveReservation(msg model.Message) error {
	return s.saveReservationRecord(msg, nil)
}

func (s Store) saveReservationRecord(msg model.Message, previous json.RawMessage) error {
	held := msg.IsQueuedForApproval()
	if held {
		if err := s.rememberApprovalHold(msg.To, 1); err != nil {
			return err
		}
	}
	if err := s.saveMessageRecord(msg, previous); err != nil {
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
func (s Store) holdForApproval(msg *model.Message) error {
	if err := s.approvalCapacity(msg.To); err != nil {
		return err
	}
	return msg.Hold()
}

// approvalCapacity checks before flood admission without changing message state.
func (s Store) approvalCapacity(targetID string) error {
	target, err := s.session(targetID)
	if err != nil {
		return err
	}
	if target.ApprovalHeld >= approvalQueueCap {
		return model.NewError(model.CodeUnavailable, "too many messages are waiting for this session's permission prompt")
	}
	return nil
}

// ApprovalHeld writers, all under the journal lock:
//   - rememberApprovalHold: +1 before a hold is written (-1 if that fails), and
//     the downward move of recountAfterScan.
//   - clearApprovalHeld: 0 after an approval scan that skipped no file.
//   - register: a new generation carries heldAfterScan(prior, settled, skipped).
//   - reconcileSessionHold (prune): the exact recount of readable holds.
//
// A scan that skipped files sets heldAfterScan(prior, processed, skipped).

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

// queuedApprovals returns approval-blocked messages for target in send order
// and the files it skipped, so a caller can report them and keep its count.
// The caller holds the journal lock.
func (s Store) queuedApprovals(target string) ([]model.Message, skippedMessages, error) {
	queued := []model.Message{}
	skipped, err := s.eachReadableMessage(func(msg model.Message) error {
		if msg.To == target && msg.IsQueuedForApproval() {
			queued = append(queued, msg)
		}
		return nil
	})
	if err != nil {
		return nil, skipped, err
	}
	sort.Slice(queued, func(i, j int) bool {
		if queued[i].CreatedAt == queued[j].CreatedAt {
			return queued[i].ID < queued[j].ID
		}
		return queued[i].CreatedAt < queued[j].CreatedAt
	})
	return queued, skipped, nil
}

func (s Store) settleQueued(msg model.Message, reason string) error {
	current, err := s.message(msg.ID)
	if err != nil {
		return err
	}
	if !current.IsQueuedForApproval() {
		return nil
	}
	if err := current.SettleNotSent(model.CodeUnavailable, reason); err != nil {
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
	if err := s.eachMessage(func(id string, msg model.Message, readErr error) error {
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
// The caller holds the journal lock. Decisions are reported in this order:
// unreadable messages, sessions in scan order, then targets with no session
// record in ID order. Each decision is reported before a dry run stops.
func (s Store) reconcileApprovalHolds(dryRun bool, report *PruneReport, retiring map[string]bool) error {
	counts, unreadable, err := s.approvalHolds()
	if err != nil {
		return err
	}
	for _, id := range unreadable {
		report.Sessions = append(report.Sessions, PruneItem{ID: id, Reason: "message_unreadable"})
	}
	sessions, issues, err := s.scanSessions()
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, issue := range issues {
		seen[issue.ID] = true
	}
	for _, v := range sessions {
		seen[v.ID] = true
		if err := s.reconcileSessionHold(v, counts[v.ID], v.Retired || retiring[v.ID], dryRun, report); err != nil {
			return err
		}
	}
	return s.settleMissingTargets(counts, seen, dryRun, report)
}

// reconcileSessionHold applies a pure count decision under the caller's lock.
// Report before a dry-run return; settle before reloading and clearing the count.
func (s Store) reconcileSessionHold(v model.Session, count int, retired, dryRun bool, report *PruneReport) error {
	decision := model.DecideApprovalHold(v.ApprovalHeld, count, retired)
	if decision.Reason == "" {
		return nil
	}
	report.Sessions = append(report.Sessions, PruneItem{ID: v.ID, Reason: decision.Reason})
	if dryRun {
		return nil
	}
	if decision.SettleQueue {
		if err := s.settleReadableApprovalQueue(v.ID, "destination has retired"); err != nil {
			return err
		}
	}
	if decision.ClearCount {
		return s.clearApprovalHeld(v.ID)
	}
	v.ApprovalHeld = count
	return s.saveSession(v)
}

// settleMissingTargets settles the queues of targets that have no session
// record, in ID order. The decision is reported before the dry-run check.
func (s Store) settleMissingTargets(counts map[string]int, seen map[string]bool, dryRun bool, report *PruneReport) error {
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

// settleApprovalQueue ends actor's queued approvals for a new registration.
// One unreadable file must not fail SessionStart, so it is skipped and
// reported. It returns the count the new registration carries: holds that may
// sit in unreadable files, which a later hook settles once they can be read.
func (s Store) settleApprovalQueue(actor, reason string, held int) (int, error) {
	settled, skipped, err := s.settleApprovalQueueRead(actor, reason)
	skipped.report(s, actor)
	return heldAfterScan(held, settled, skipped.count), err
}

// settleReadableApprovalQueue settles queued messages and ignores files prune
// has already reported as unreadable, so one bad file cannot abort cleanup.
func (s Store) settleReadableApprovalQueue(actor, reason string) error {
	_, _, err := s.settleApprovalQueueRead(actor, reason)
	return err
}

// settleApprovalQueueRead settles every readable queued approval of actor and
// returns how many it settled and the files it could not read.
func (s Store) settleApprovalQueueRead(actor, reason string) (int, skippedMessages, error) {
	queued, skipped, err := s.queuedApprovals(actor)
	if err != nil {
		return 0, skipped, err
	}
	for i, msg := range queued {
		if err := s.settleQueued(msg, reason); err != nil {
			return i, skipped, err
		}
	}
	return len(queued), skipped, nil
}

type approvalResolution struct {
	jobs        []model.Message
	unreachable bool
}

// resolveApprovalQueue owns settlement/adoption under the journal lock.
// A hook poll preserves an outstanding approval count until the prompt resolves;
// lifecycle release also repairs an empty count while approval is still open.
func (s Store) resolveApprovalQueue(current model.Session, hookPoll bool) (approvalResolution, error) {
	reason := ""
	if current.Retired {
		reason = "destination has retired"
	} else if s.sessionLiveness(current) == model.LivenessExited {
		reason = "destination owner process has exited"
	}
	result := approvalResolution{unreachable: reason != ""}
	if current.ApprovalHeld == 0 || (hookPoll && reason == "" && current.State == model.StateApproval) {
		return result, nil
	}
	// Every lifecycle hook reaches this scan while approvals are held. One
	// unreadable file must not block it; see heldAfterScan for the count.
	queued, skipped, err := s.queuedApprovals(current.ID)
	if err != nil {
		return result, err
	}
	skipped.report(s, current.ID)
	if reason != "" {
		for _, msg := range queued {
			if err := s.settleQueued(msg, reason); err != nil {
				return result, err
			}
		}
	} else if len(queued) != 0 {
		if current.State == model.StateApproval {
			return result, nil
		}
		result.jobs, err = s.releaseReadyApprovals(current, queued)
		if err != nil {
			return result, err
		}
	}
	return result, recountAfterScan(s, current, len(queued), skipped.count)
}

// heldAfterScan is the queued-approval count after a scan that resolved
// processed readable holds (released or settled) and could not read skipped
// files. Only unreadable files can still hold approvals, so the count never
// exceeds skipped, and every resolved hold is subtracted, so it never grows.
// With nothing skipped it is 0, the exact count.
func heldAfterScan(prior, processed, skipped int) int {
	return min(max(0, prior-processed), skipped)
}

// recountAfterScan stores heldAfterScan for current under the caller's lock. A
// healthy scan clears the count as before; otherwise the count moves only down.
func recountAfterScan(s Store, current model.Session, processed, skipped int) error {
	if skipped == 0 {
		return s.clearApprovalHeld(current.ID)
	}
	return s.rememberApprovalHold(current.ID, heldAfterScan(current.ApprovalHeld, processed, skipped)-current.ApprovalHeld)
}

func (s Store) releaseReadyApprovals(current model.Session, queued []model.Message) ([]model.Message, error) {
	var jobs []model.Message
	next := model.StatusAttempting
	if current.Receive.HookDelivery {
		next = model.StatusPendingActivity
	}
	for _, msg := range queued {
		if msg.TargetGeneration != current.Generation {
			if err := s.settleQueued(msg, "destination registration changed before delivery"); err != nil {
				return nil, err
			}
			continue
		}
		if err := msg.ReleaseHeld(next); err != nil {
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
	var jobs []model.Message
	var target model.Session
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
