package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// CursorWakeHint binds a native listener notification to a message and receiving
// registration. Peer text and credentials never enter this wire contract.
type CursorWakeHint struct {
	MessageID  string `json:"message_id"`
	Generation string `json:"generation"`
}

func (s Store) cursorListenerLockPath(actor string) string {
	return filepath.Join(s.Dir, "cursor-listener-"+strings.TrimPrefix(actor, "cursor:")+".lock")
}

// ListenCursor must be started by the current Cursor model as one native Shell
// background task with notify_on_output matching "ZALKI_WAKE ". Only Cursor
// can register that native task; starting this from a hook would not wake a model.
// It blocks on socket accept and consumes no model turns while there is no mail.
func (s Store) ListenCursor(ctx context.Context, actor, key string, output io.Writer) (resultErr error) {
	armed := false
	defer func() {
		level := slog.LevelInfo
		event := "listener.stopped"
		if !armed {
			event = "listener.start_failed"
		}
		if resultErr != nil {
			level = slog.LevelWarn
		}
		s.logEvent(level, event, eventData{Actor: actor, Err: resultErr})
	}()
	var registration Session
	err := s.locked(func() error {
		var err error
		registration, err = s.authenticate(actor, key)
		if err == nil && registration.Kind != "cursor" {
			return errors.New("native Cursor listener requires your own Cursor identity")
		}
		return err
	})
	if err != nil {
		return err
	}
	// A separate nonblocking lifetime lock prevents duplicate listeners. Never
	// hold the journal lock while waiting for native input or socket IO.
	fd, err := syscall.Open(s.cursorListenerLockPath(actor), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer syscall.Close(fd) //nolint:errcheck // Closing also releases the listener lifetime lock.
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("this Cursor conversation already has a listener; keep its original native Shell task")
	}
	defer syscall.Flock(fd, syscall.LOCK_UN) //nolint:errcheck // Descriptor close also releases the lock.
	// Keep below macOS's Unix-socket path limit, independent of long home/TMPDIR
	// paths. Each listener owns a fresh private directory and removes only that.
	dir, err := os.MkdirTemp("/tmp", "zalki-cursor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // Best-effort cleanup of this listener's private temporary directory.
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "inbox.sock"), Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close() //nolint:errcheck // Cancellation may already have closed the listener.
	if err := os.Chmod(listener.Addr().String(), 0600); err != nil {
		return err
	}
	endpoint, err := InspectEndpoint(listener.Addr().String())
	if err != nil {
		return err
	}
	err = s.locked(func() error {
		current, err := s.authenticate(actor, key)
		if err != nil {
			return err
		}
		if current.Generation != registration.Generation {
			return errors.New("Cursor registration changed while arming its listener")
		}
		state := cursorState(current)
		state.Wake = endpoint
		setCursorState(&current, state)
		return s.saveSession(current)
	})
	if err != nil {
		return err
	}
	defer s.clearCursorWake(actor, registration.Generation, endpoint)
	stopClose := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopClose()
	if _, err := fmt.Fprintln(output, "ZALKI_LISTENER_READY", actor); err != nil {
		return err
	}
	// A process that cannot write to its native task output never completed
	// setup. Keep the recovery guard in that case instead of retrying forever.
	if err := s.locked(func() error {
		current, err := s.session(actor)
		if err != nil {
			return err
		}
		if current.Generation != registration.Generation || cursorState(current).Wake == nil || *cursorState(current).Wake != *endpoint {
			return errors.New("Cursor registration changed while completing listener setup")
		}
		state := cursorState(current)
		state.RecoveryAttempted = false
		setCursorState(&current, state)
		return s.saveSession(current)
	}); err != nil {
		return err
	}
	armed = true
	s.logEvent(slog.LevelInfo, "listener.armed", eventData{Actor: actor, Generation: registration.Generation})
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		err = s.consumeCursorHint(conn, actor, registration.Generation, endpoint, output)
		_ = conn.Close() // The wake attempt has already completed.
		if errors.Is(err, errCursorOutput) {
			return err
		}
		if err != nil {
			s.logEvent(slog.LevelDebug, "listener.hint_rejected", eventData{Actor: actor, Err: err})
		}
		// Invalid or stale hints cannot terminate the valid listener, and neither
		// their content nor their errors are printed into the model's inbox.
	}
}

var errCursorOutput = errors.New("Cursor listener output failed")

func (s Store) consumeCursorHint(conn net.Conn, actor, generation string, endpoint *Endpoint, output io.Writer) error {
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	var hint CursorWakeHint
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&hint); err != nil {
		return err
	}
	if hint.Generation != generation {
		return errors.New("stale Cursor wake generation")
	}
	var alreadyACKed bool
	err := s.locked(func() error {
		current, err := s.session(actor)
		if err != nil {
			return err
		}
		if current.Retired || current.Generation != generation || cursorState(current).Wake == nil || *cursorState(current).Wake != *endpoint || current.State == StateApproval {
			return errors.New("Cursor listener is no longer the active recipient")
		}
		message, err := s.message(hint.MessageID)
		if err != nil {
			return err
		}
		if message.To != actor || message.TargetGeneration != generation || (!hookPendingStatus(message.Status) && message.Status != StatusContextEmitted) {
			return errors.New("hint does not refer to this recipient's pending message")
		}
		alreadyACKed = message.AcknowledgedAt != ""
		return nil
	})
	if err != nil {
		return err
	}
	if !alreadyACKed {
		// Only validated IDs enter notifications; peer text stays in the journal.
		if _, err := fmt.Fprintln(output, "ZALKI_WAKE", hint.MessageID); err != nil {
			return fmt.Errorf("%w: %w", errCursorOutput, err)
		}
		s.logEvent(slog.LevelInfo, "listener.wake_emitted", eventData{Actor: actor, MessageID: hint.MessageID, Generation: generation})
	}
	return json.NewEncoder(conn).Encode(hint)
}

func (s Store) clearCursorWake(actor, generation string, endpoint *Endpoint) {
	err := s.locked(func() error {
		current, err := s.session(actor)
		if err != nil {
			return err
		}
		if current.Generation != generation || cursorState(current).Wake == nil || *cursorState(current).Wake != *endpoint {
			return nil
		}
		state := cursorState(current)
		state.Wake = nil
		setCursorState(&current, state)
		return s.saveSession(current)
	})
	if err != nil {
		s.logEvent(slog.LevelWarn, "listener.cleanup_failed", eventData{Actor: actor, Err: err})
	}
}
