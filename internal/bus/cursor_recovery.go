package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"

	"github.com/zalkilabs/zalki/internal/install"
)

// A socket left behind by a crash is insufficient evidence of a live listener.
// Check the bound endpoint and its existing lifetime lock without sending hints
// or starting a process outside Cursor's native background-task registry.
func (s Store) cursorListenerRunning(v Session) bool {
	if CheckEndpoint(cursorState(v).Wake) != nil {
		return false
	}
	fd, err := syscall.Open(s.cursorListenerLockPath(v.ID), syscall.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd) //nolint:errcheck // Descriptor cleanup after the liveness check.
	err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(fd, syscall.LOCK_UN) // Closing the descriptor also releases the lock.
		return false
	}
	return errors.Is(err, syscall.EWOULDBLOCK)
}

// Called only by trusted lifecycle hooks. One recovery context is allowed per
// unarmed interval, not per tool call or stop iteration. A denied setup cannot
// turn into an automatic retry loop. Successful listener registration resets it.
func (s Store) cursorRecoveryContext(actor string) (string, error) {
	binary, err := os.Executable()
	if err != nil {
		return "", err
	}
	var key string
	err = s.locked(func() error {
		v, err := s.session(actor)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if v.Kind != "cursor" || v.Retired || processLiveness(v.Owner) == "exited" || v.State == StateApproval || cursorState(v).RecoverySuppressed || s.cursorListenerRunning(v) {
			return nil
		}
		if cursorState(v).RecoveryAttempted {
			return nil
		}
		// Only the hash is stored. Reissue this session's own capability rather
		// than recover plaintext from terminal logs or another session. Retain
		// actor, generation, and pending messages across the resumed conversation.
		key = newID() + newID()
		hash := sha256.Sum256([]byte(key))
		v.KeyHash = hex.EncodeToString(hash[:])
		state := cursorState(v)
		state.RecoveryAttempted, state.Wake = true, nil
		setCursorState(&v, state)
		v.Receive = cursorDeferredReceive()
		return s.saveSession(v)
	})
	if err != nil || key == "" {
		return "", err
	}
	s.logEvent(slog.LevelInfo, "listener.recovery_requested", eventData{Actor: actor})
	command := "ZALKI_CAPABILITY=" + install.ShellQuote(key) + " " + install.ShellQuote(binary) + " --state " + install.ShellQuote(s.Dir) + " cursor-listen --actor " + install.ShellQuote(actor)
	return fmt.Sprintf("[Zalki local listener recovery]\nYour registered Cursor session has no verified live listener. "+
		"Your actor remains %s. Your refreshed private capability is %s; use it for future zalki calls. "+
		"This replaces only your own previous capability; never reveal it or borrow another session's. "+
		"If a listener setup command was already denied, do not retry it: report that and keep deferred hook delivery. "+
		"Otherwise, under your normal permissions, arm one native Shell background task with block_until_ms=0 and "+
		"notify_on_output={pattern:'ZALKI_WAKE ',reason:'zalki message arrived'} running: `%s`. "+
		"If your native task registry shows an old unusable cursor-listen task for this exact actor, cancel only that task before replacing it. "+
		"Do not inspect terminal logs, read credential files, poll, wait for output, change permissions, or start another listener after a refusal. "+
		"If setup is unavailable or denied, report it and retain deferred delivery. End your turn normally. "+
		"On ZALKI_WAKE, read list_messages with that message_id; use reply_message to acknowledge and reply, or ack_message when no reply is needed. "+
		"This setup instruction is emitted once for this missing-listener interval; it is not an agent request.", actor, key, command), nil
}

func (s Store) suppressCursorRecovery(actor string) error {
	return s.locked(func() error {
		v, err := s.session(actor)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		v.State, v.UpdatedAt = StateReady, timestamp()
		state := cursorState(v)
		state.RecoverySuppressed = true
		setCursorState(&v, state)
		return s.saveSession(v)
	})
}

func appendContext(context, extra string) string {
	return strings.TrimSpace(context + "\n\n" + extra)
}
