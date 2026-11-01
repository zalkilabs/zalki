package bus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

// watchTTL is how long a one-shot completion watch stays armed.
const watchTTL = 12 * time.Hour

// noticeKind marks an automatic completion notice so it cannot be mistaken for
// a message the target chose to send.
const noticeKind = "notice"

// statusLineLimit is the maximum status text copied from a Stop hook.
const statusLineLimit = 200

// WatchResult is the watch_agent tool result. A watching result arms one
// notice. An ended result answers a session that has already exited or retired.
type WatchResult struct {
	Status    string `json:"status"`
	To        string `json:"to"`
	Event     string `json:"event,omitempty"`
	At        string `json:"at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

// Watch is one pending notice from Watcher to Target.
type Watch struct {
	Schema    int    `json:"schema"`
	Watcher   string `json:"watcher"`
	Target    string `json:"target"`
	CreatedAt string `json:"created_at,omitempty"`
	ExpiresAt string `json:"expires_at"`
}

// Watch records or refreshes one completion notice for actor about to.
// A retired or exited target is answered immediately and stores nothing.
func (s *Service) Watch(actor, key, to string) (WatchResult, error) {
	var result WatchResult
	err := s.Store.locked(func() error {
		watcher, err := s.Store.authenticate(actor, key)
		if err != nil {
			return err
		}
		target, err := s.Store.resolve(to)
		if err != nil {
			return err
		}
		if watcher.ID == target.ID {
			return fail(CodeInvalidInput, "watching your own session is not supported")
		}
		result.To = target.ID
		if target.Retired || s.Store.sessionLiveness(target) == LivenessExited {
			result.Status = "ended"
			result.Event = "ended"
			result.At = target.UpdatedAt
			if result.At == "" {
				result.At = timestamp()
			}
			return nil
		}
		now := time.Now().UTC()
		expires := now.Add(watchTTL).Format(time.RFC3339Nano)
		if err := s.Store.saveWatch(Watch{Schema: recordSchema, Watcher: watcher.ID, Target: target.ID, CreatedAt: now.Format(time.RFC3339Nano), ExpiresAt: expires}); err != nil {
			return err
		}
		result.Status = "watching"
		result.ExpiresAt = expires
		return nil
	})
	return result, err
}

func (s Store) watchPath(watcher, target string) (string, error) {
	if _, err := s.sessionPath(watcher); err != nil {
		return "", err
	}
	if _, err := s.sessionPath(target); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, "watches", strings.ReplaceAll(target, ":", "_"), strings.ReplaceAll(watcher, ":", "_")+".json"), nil
}

func (s Store) saveWatch(v Watch) error {
	path, err := s.watchPath(v.Watcher, v.Target)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	v.Schema = recordSchema
	return writeJSON(path, v)
}

// consumeWatches removes due watches on target. A watch created after asOf stays
// armed so a Stop already in progress cannot consume a subscription to the next turn.
// Expired watches are removed and not returned.
func (s Store) consumeWatches(target string, asOf time.Time) ([]Watch, error) {
	var due []Watch
	err := s.locked(func() error {
		if _, err := s.sessionPath(target); err != nil {
			return err
		}
		dir := filepath.Join(s.Dir, "watches", strings.ReplaceAll(target, ":", "_"))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		now := time.Now()
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			var watch Watch
			if err := readRecord(path, &watch, &watch.Schema); err != nil {
				return err
			}

			expires, err := time.Parse(time.RFC3339Nano, watch.ExpiresAt)
			if err != nil || !expires.After(now) || watch.Target != target {
				if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return removeErr
				}
				continue
			}
			if watch.CreatedAt != "" {
				created, createdErr := time.Parse(time.RFC3339Nano, watch.CreatedAt)
				if createdErr == nil && created.After(asOf) {
					continue
				}
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			due = append(due, watch)
		}
		return nil
	})
	return due, err
}

func completionKind(event HookInput) (string, bool) {
	switch event.Event {
	case "SessionEnd":
		return "ended", true
	case "Stop":
		if event.Status != "" && event.Status != "completed" {
			return "", false
		}
		return "idle", true
	default:
		return "", false
	}
}

func statusLine(text string) string {
	if strings.ContainsRune(text, 0) {
		return ""
	}
	text = strings.Join(strings.Fields(text), " ")
	if text == "" || len(text) <= statusLineLimit {
		return text
	}
	const ellipsis = "…"
	budget := statusLineLimit - len(ellipsis)
	cut := budget
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	if space := strings.LastIndexByte(text[:cut], ' '); space >= budget/2 {
		cut = space
	}
	return text[:cut] + ellipsis
}

func completionBody(subject Session, event, at, line string) string {
	name := subject.Name
	if !validName(name) {
		name = "unnamed"
	}
	verb := "finished a turn"
	if event == "ended" {
		verb = "ended the session"
	}
	body := fmt.Sprintf("Zalki notice: %s (%s) %s at %s", name, subject.ID, verb, at)
	if line != "" {
		body += "\n" + line
	}
	return body
}

func (s *Service) notifyWatches(ctx context.Context, actor string, event HookInput, asOf time.Time) error {
	kind, ok := completionKind(event)
	if !ok {
		return nil
	}
	watches, err := s.Store.consumeWatches(actor, asOf)
	if err != nil || len(watches) == 0 {
		return err
	}
	subject, err := s.Store.session(actor)
	if err != nil {
		return err
	}
	body := completionBody(subject, kind, timestamp(), statusLine(event.LastAssistantMessage))
	var first error
	for _, watch := range watches {
		if _, err := s.deliverCompletion(ctx, watch.Watcher, subject, body); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Service) deliverCompletion(ctx context.Context, watcherID string, subject Session, body string) (Message, error) {
	var msg Message
	var target Session
	var fresh bool
	err := s.Store.locked(func() error {
		current, err := s.Store.session(watcherID)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		target = current
		msg = Message{
			Schema: recordSchema, ID: newID(), Kind: noticeKind, From: subject.ID, FromName: subject.Name,
			FromLabel: "Zalki", To: current.ID, Body: body,
			TargetGeneration: current.Generation, CreatedAt: timestamp(), Status: StatusAttempting,
		}
		if current.State == StateApproval {
			if err := s.Store.holdForApproval(&msg); err != nil {
				if moveErr := msg.settleNotSent(ErrorCodeOf(err), err.Error()); moveErr != nil {
					return moveErr
				}
			}
		} else {
			fresh = true
		}
		return s.Store.saveReservation(msg)
	})
	if err != nil || !fresh || msg.ID == "" {
		return msg, err
	}
	return s.deliverReserved(ctx, msg, target)
}

// NoticeDeliveryError reports best-effort notice failures separately from
// journal observation errors. CLI diagnostics can warn without hiding results.
type NoticeDeliveryError struct{ Err error }

func (e *NoticeDeliveryError) Error() string {
	return "completion notice delivery failed: " + e.Err.Error()
}
func (e *NoticeDeliveryError) Unwrap() error { return e.Err }

// NoticeExitedSessions delivers an ended notice for each unexpired watch whose
// target is retired or has positive exit evidence. List and prune call it
// when they observe that exit. An expired watch is removed and not delivered.
func (s *Service) NoticeExitedSessions(ctx context.Context) error {
	var exited []Session
	err := s.Store.locked(func() error {
		sessions, err := s.Store.rawSessions()
		if err != nil {
			return err
		}
		for _, v := range sessions {
			if !v.Retired && s.Store.sessionLiveness(v) != LivenessExited {
				continue
			}
			exited = append(exited, v)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var first error
	for _, subject := range exited {
		watches, err := s.Store.consumeWatches(subject.ID, time.Now())
		if err != nil {
			return err
		}
		body := completionBody(subject, "ended", timestamp(), "")
		for _, watch := range watches {
			message, err := s.deliverCompletion(ctx, watch.Watcher, subject, body)
			if err == nil && message.Receipt != nil && message.Receipt.Error != "" && !message.IsQueuedForApproval() {
				err = fail(CodeUnavailable, "completion notice was not delivered")
			}
			if err != nil && first == nil {
				first = err
			}
		}
	}
	if first != nil {
		return &NoticeDeliveryError{Err: first}
	}
	return nil
}
