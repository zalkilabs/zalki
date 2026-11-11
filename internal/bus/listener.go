package bus

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// Lease is one process's exclusive ownership of a registration's listener. It
// holds the nonblocking lifetime lock for its whole life and never holds the
// journal lock while waiting on that lock or on socket IO. Lock order is
// always lifetime lock first, then journal lock.
//
// A Lease is used by one goroutine. Every journal operation re-checks that the
// session still records this lease's ID, generation and endpoint, so a lease
// superseded by recovery, re-registration or another listener cannot confirm,
// validate or clear anything.
type Lease struct {
	store                     Store
	actor, kind, key, id, gen string
	fd                        int
	endpoint                  *model.Endpoint
	confirmed, closed         bool
}

func (s Store) listenerLockPath(kind, actor string) string {
	return filepath.Join(s.Dir, kind+"-listener-"+strings.TrimPrefix(actor, kind+":")+".lock")
}

// removeFreeListenerLock reports whether the listener lock file is present and
// unlocked. The journal lock is already held, so the flock is nonblocking and
// cannot deadlock a listener that takes the locks in the opposite order.
// remove unlinks the file only in that case.
func (s Store) removeFreeListenerLock(kind, actor string, remove bool) (bool, error) {
	path := s.listenerLockPath(kind, actor)
	fd, err := journal.OpenLockFile(path, false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer journal.CloseLockFile(fd) //nolint:errcheck // Descriptor cleanup after the nonblocking lock attempt.
	if err := journal.LockExclusive(fd, true); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	defer journal.Unlock(fd) //nolint:errcheck // Closing the descriptor also releases the lock.
	if !remove {
		return true, nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// lockHeld reports whether some process holds the registration's lifetime lock.
// A missing or unlocked lock file is not evidence of a live listener.
func (s Store) lockHeld(v model.Session) bool {
	fd, err := journal.OpenLockFile(s.listenerLockPath(v.Kind, v.ID), false)
	if err != nil {
		return false
	}
	defer journal.CloseLockFile(fd) //nolint:errcheck // Descriptor cleanup after the liveness check.
	err = journal.LockExclusive(fd, true)
	if err == nil {
		_ = journal.Unlock(fd) // Closing the descriptor also releases the lock.
		return false
	}
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// listenerStatus checks the lease against its bound endpoint and the lifetime
// lock. A socket left behind by a crash is insufficient evidence of a listener.
func (s Store) listenerStatus(v model.Session) model.ListenerStatus {
	held := s.lockHeld(v)
	switch {
	case v.Listener != nil && held && v.Listener.Generation == v.Generation && CheckEndpoint(&v.Listener.Endpoint) == nil:
		return model.ListenerRunning
	case v.Listener != nil:
		return model.ListenerStale
	case held:
		return model.ListenerUnleased
	}
	return model.ListenerNone
}

// AcquireListenerLease authenticates the registration's own capability and
// takes its lifetime lock. Only one lease per registration can exist at a time.
func (s Store) AcquireListenerLease(actor, key string) (lease *Lease, err error) {
	defer func() {
		if err != nil {
			s.logEvent(slog.LevelWarn, "listener.start_failed", eventData{Actor: actor, Err: err})
		}
	}()
	var v model.Session
	err = s.locked(func() error {
		var err error
		if v, err = s.authenticate(actor, key); err != nil {
			return err
		}
		adapter, err := s.registry().Lookup(v.Kind)
		if err != nil {
			return err
		}
		if !adapter.ReceiveCapabilities(v).RequiresListener {
			return model.NewError(model.CodeInvalidInput, "a native listener requires your own listener-capable identity")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	fd, err := acquireListenerLock(s.listenerLockPath(v.Kind, actor), journal.OpenLockFile)
	if err != nil {
		return nil, err
	}
	return &Lease{store: s, actor: actor, kind: v.Kind, key: key, id: newID(), gen: v.Generation, fd: fd}, nil
}

// acquireListenerLock flocks path and keeps the descriptor only while it still
// refers to the directory entry. Prune can unlink the file after open and
// before flock; the flock would then succeed on an unlinked inode and a second
// acquire would lock a new file at the same path.
func acquireListenerLock(path string, open func(string, bool) (int, error)) (int, error) {
	var last error
	for range 3 {
		fd, err := open(path, true)
		if err != nil {
			return -1, err
		}
		if err := journal.LockExclusive(fd, true); err != nil {
			_ = journal.CloseLockFile(fd)
			return -1, model.NewError(model.CodeConflict, "this conversation already has a listener; keep its original native task")
		}
		linked, err := lockFileStillLinked(fd, path)
		if err != nil {
			_ = journal.CloseLockFile(fd)
			return -1, err
		}
		if linked {
			return fd, nil
		}
		_ = journal.CloseLockFile(fd)
		last = model.NewError(model.CodeConflict, "listener lock changed while it was being acquired")
	}
	return -1, last
}

func lockFileStillLinked(fd int, path string) (bool, error) {
	return journal.Linked(fd, path)
}

// Generation is the registration generation this lease was acquired for.
func (l *Lease) Generation() string { return l.gen }

func (l *Lease) owns(v model.Session) bool {
	return l.endpoint != nil && v.Generation == l.gen && v.Listener != nil &&
		v.Listener.ID == l.id && v.Listener.Generation == l.gen && v.Listener.Endpoint == *l.endpoint
}

// Arm records endpoint as the registration's listener. It re-authenticates the
// capability, so a credential rotated since acquisition cannot arm, even when
// the generation is unchanged. Any stale lease record is replaced: holding the
// lifetime lock proves no other lease is alive.
func (l *Lease) Arm(endpoint *model.Endpoint) error {
	if l.closed || l.endpoint != nil {
		return errors.New("listener lease is closed or already armed")
	}
	if err := CheckEndpoint(endpoint); err != nil {
		return err
	}
	bound := *endpoint
	err := l.store.locked(func() error {
		current, err := l.store.authenticate(l.actor, l.key)
		if err != nil {
			return err
		}
		if current.Generation != l.gen {
			return model.NewError(model.CodeConflict, "registration changed while arming its listener")
		}
		current.Listener = &model.ListenerLease{ID: l.id, Generation: l.gen, Endpoint: bound}
		return l.store.saveSession(current)
	})
	if err == nil {
		l.endpoint = &bound
	}
	return err
}

// Confirm completes setup after the listener announced itself, applying the
// adapter's next state in the same save. A listener that could not announce
// itself never confirms, so the adapter's recovery guard stays in place.
func (l *Lease) Confirm(policy func(model.SessionView) (json.RawMessage, error)) error {
	err := l.store.locked(func() error {
		current, err := l.store.session(l.actor)
		if err != nil {
			return err
		}
		if !l.owns(current) {
			return model.NewError(model.CodeConflict, "registration changed while completing listener setup")
		}
		next, err := policy(viewOf(current))
		if err != nil {
			return err
		}
		if err := l.store.applyAdapterState(&current, next); err != nil {
			return err
		}
		return l.store.saveSession(current)
	})
	if err != nil {
		return err
	}
	l.confirmed = true
	l.store.logEvent(slog.LevelInfo, "listener.armed", eventData{Actor: l.actor, Generation: l.gen})
	return nil
}

// ValidateDelivery checks that this lease is still the active recipient and
// that messageID is one of its registration's pending messages. It reports
// whether the message was already acknowledged.
func (l *Lease) ValidateDelivery(messageID string) (acknowledged bool, err error) {
	err = l.store.readLocked(func() error {
		current, err := l.store.session(l.actor)
		if err != nil {
			return err
		}
		if current.Retired || !l.owns(current) || current.State == model.StateApproval {
			return errors.New("listener is no longer the active recipient")
		}
		message, err := l.store.message(messageID)
		if err != nil {
			return err
		}
		if message.To != l.actor || message.TargetGeneration != l.gen || (!model.HookPendingStatus(message.Status) && message.Status != model.StatusContextEmitted) {
			return errors.New("hint does not refer to this recipient's pending message")
		}
		acknowledged = message.AcknowledgedAt != ""
		return nil
	})
	return acknowledged, err
}

// RejectHint records an invalid or stale wake attempt without exposing it to the model.
func (l *Lease) RejectHint(err error) {
	l.store.logEvent(slog.LevelDebug, "listener.hint_rejected", eventData{Actor: l.actor, Err: err})
}

// WakeEmitted records a wake notification written for a validated message.
func (l *Lease) WakeEmitted(messageID string) {
	l.store.logEvent(slog.LevelInfo, "listener.wake_emitted", eventData{Actor: l.actor, MessageID: messageID, Generation: l.gen})
}

// releaseListenerLease drops the persisted lease. A process that still holds
// the lifetime lock is fenced: owns requires the record to name its lease, so
// that process cannot confirm, validate, or clear the record.
func releaseListenerLease(v *model.Session) {
	v.Listener = nil
}

// Close clears the lease record only while it still records this lease's ID,
// generation and endpoint, then releases the lifetime lock either way. It is
// idempotent; cause is the listener's exit error.
func (l *Lease) Close(cause error) {
	if l.closed {
		return
	}
	l.closed = true
	if l.endpoint != nil {
		err := l.store.locked(func() error {
			current, err := l.store.session(l.actor)
			if err != nil {
				return err
			}
			if !l.owns(current) {
				return nil
			}
			releaseListenerLease(&current)
			return l.store.saveSession(current)
		})
		if err != nil {
			l.store.logEvent(slog.LevelWarn, "listener.cleanup_failed", eventData{Actor: l.actor, Err: err})
		}
	}
	_ = journal.Unlock(l.fd) // Closing the descriptor also releases the lock.
	_ = journal.CloseLockFile(l.fd)
	level, event := slog.LevelInfo, "listener.stopped"
	if !l.confirmed {
		event = "listener.start_failed"
	}
	if cause != nil {
		level = slog.LevelWarn
	}
	l.store.logEvent(level, event, eventData{Actor: l.actor, Err: cause})
}
