package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"os"
	"syscall"
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
func (s Store) claimCursorRecovery(actor string) (CursorRecovery, error) {
	var key string
	err := s.locked(func() error {
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
		return s.saveSession(v)
	})
	if err != nil || key == "" {
		return CursorRecovery{}, err
	}
	s.logEvent(slog.LevelInfo, "listener.recovery_requested", eventData{Actor: actor})
	return CursorRecovery{Actor: actor, Capability: key, StateDir: s.Dir}, nil
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
