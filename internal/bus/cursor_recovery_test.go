package bus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestCursorRecoverySkipsRetiredUnknownAndApprovalSessions(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	for _, state := range []string{"approval", "retired"} {
		if err := store.Update(to.ID, SessionState(state), state == "retired"); err != nil {
			t.Fatal(err)
		}
		if text, err := store.claimCursorRecovery(to.ID); err != nil || text.Capability != "" {
			t.Fatalf("recovered a %s session: %v", state, err)
		}
	}
	if text, err := store.claimCursorRecovery("cursor:" + newID()); err != nil || text.Capability != "" {
		t.Fatal("recovery registered an unknown session", err)
	}
	current, err := store.session(to.ID)
	if err != nil || current.KeyHash != to.KeyHash {
		t.Fatal("skipped recovery changed credentials", err)
	}
}

func TestConcurrentCursorRecoveryClaimsOnlyOneContext(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	var wg sync.WaitGroup
	contexts := make(chan CursorRecovery, 12)
	errors := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, err := store.claimCursorRecovery(to.ID)
			contexts <- text
			errors <- err
		}()
	}
	wg.Wait()
	close(contexts)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for text := range contexts {
		if text.Capability != "" {
			count++
			if _, err := store.authenticate(to.ID, text.Capability); err != nil {
				t.Fatal("concurrent claim replaced the emitted capability", err)
			}
		}
	}
	if count != 1 {
		t.Fatalf("concurrent hooks emitted %d recovery contexts", count)
	}
}

func TestCursorRecoveryDoesNotTrustSocketWithoutLifetimeLock(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	dir, err := os.MkdirTemp("/tmp", "ab-recovery-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "inbox.sock"), Net: "unix"})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("execution sandbox forbids binding a Unix socket: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	endpoint, err := InspectEndpoint(listener.Addr().String())
	state := cursorState(to)
	state.Wake = endpoint
	setCursorState(&to, state)
	if err != nil {
		t.Fatal(err)
	}
	to.Receive = ReceiveCapabilities{IdleWake: true, Delivery: "native_background_shell_output"}
	if err := store.locked(func() error { return store.saveSession(to) }); err != nil {
		t.Fatal(err)
	}
	// The socket itself is real and bound, but no authenticated listener owns
	// its lifetime lock. Neither a missing lock nor an unlocked file is proof.
	for _, createLock := range []bool{false, true} {
		if createLock {
			if err := os.WriteFile(store.cursorListenerLockPath(to.ID), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if store.cursorListenerRunning(to) {
			t.Fatal("trusted an unrelated socket without listener lock ownership")
		}
	}
	text, err := store.claimCursorRecovery(to.ID)
	if err != nil || text.Capability == "" {
		t.Fatal("stale listener did not trigger recovery", err)
	}
	current, err := store.session(to.ID)
	if err != nil || cursorState(current).Wake != nil || current.Receive.IdleWake {
		t.Fatal("recovery retained a false idle-wake advertisement", err)
	}
}

func TestCursorRecoveryClaimPreservesRegistrationAndPendingMessages(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, oldKey := register(t, store, "cursor")
	message, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "pending during recovery"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(message.ID, NewReceipt(StatusPendingActivity)); err != nil {
		t.Fatal(err)
	}
	claim, err := store.claimCursorRecovery(to.ID)
	if err != nil || claim.Actor != to.ID || claim.StateDir != store.Dir || claim.Capability == "" {
		t.Fatal("missing receiver-owned recovery claim", err)
	}
	current, err := store.authenticate(to.ID, claim.Capability)
	if err != nil || current.Generation != to.Generation || NativeID(current) != NativeID(to) || !cursorState(current).RecoveryAttempted || current.Receive.IdleWake {
		t.Fatal("recovery changed registration or lost the atomic claim", err)
	}
	if _, err := store.authenticate(to.ID, oldKey); err == nil {
		t.Fatal("old capability survived recovery")
	}
	pending, err := store.message(message.ID)
	if err != nil || pending.TargetGeneration != to.Generation || pending.AcknowledgedAt != "" || pending.Status != StatusPendingActivity {
		t.Fatal("recovery changed a pending message", err)
	}
	path, err := store.sessionPath(to.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(record), claim.Capability) {
		t.Fatal("plaintext recovery capability reached the journal", err)
	}
	encoded, err := json.Marshal(claim)
	if err != nil || strings.Contains(string(encoded), claim.Capability) {
		t.Fatal("serializing a recovery result exposed its capability", err)
	}
}

type failedRecoveryOutput struct{}

func (failedRecoveryOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestCursorRecoveryOutputFailureDoesNotLoop(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	text, err := store.claimCursorRecovery(to.ID)
	if err != nil {
		t.Fatal(err)
	}
	err = store.ListenCursor(context.Background(), to.ID, text.Capability, failedRecoveryOutput{})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("execution sandbox forbids binding a Unix socket: %v", err)
	}
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("expected native output failure, got %v", err)
	}
	if text, err := store.claimCursorRecovery(to.ID); err != nil || text.Capability != "" {
		t.Fatal("failed task output triggered an automatic setup loop", err)
	}
	current, err := store.session(to.ID)
	if err != nil || cursorState(current).Wake != nil || current.Receive.IdleWake {
		t.Fatal("failed task still advertised a live listener", err)
	}
}
