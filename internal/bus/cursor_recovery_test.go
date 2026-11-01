package bus

import (
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
)

func TestCursorRecoverySkipsRetiredUnknownAndApprovalSessions(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	for _, state := range []string{"approval", "retired"} {
		if err := store.Update(to.ID, SessionState(state), state == "retired"); err != nil {
			t.Fatal(err)
		}
		if text, err := claimCursorRecovery(store, to.ID); err != nil || text.Capability != "" {
			t.Fatalf("recovered a %s session: %v", state, err)
		}
	}
	if text, err := claimCursorRecovery(store, "cursor:"+newID()); err != nil || text.Capability != "" {
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
	contexts := make(chan RecoveryGrant, 12)
	errors := make(chan error, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			text, err := claimCursorRecovery(store, to.ID)
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

// A crash leaves a lease record whose socket may still be bound. Without the
// lifetime lock it is not a listener: recovery proceeds, and the reissued
// capability retires the lease, returning the record to the older schema.
func TestCursorRecoveryRetiresAStaleLease(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	to.Listener = &ListenerLease{ID: newID(), Generation: to.Generation, Endpoint: *listenSocket(t)}
	if err := store.locked(func() error { return store.saveSession(to) }); err != nil {
		t.Fatal(err)
	}
	if recordSchemaOf(t, store, to.ID) != leaseSessionSchema {
		t.Fatal("fixture lease was not written under its schema")
	}
	for _, createLock := range []bool{false, true} {
		if createLock {
			if err := os.WriteFile(store.listenerLockPath(to.Kind, to.ID), nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if store.listenerStatus(to) == ListenerRunning {
			t.Fatal("trusted a bound socket without listener lock ownership")
		}
	}
	grant, err := claimCursorRecovery(store, to.ID)
	if err != nil || grant.Capability == "" {
		t.Fatal("stale lease did not trigger recovery", err)
	}
	current, err := store.session(to.ID)
	if err != nil || current.Listener != nil || current.Receive.IdleWake || recordSchemaOf(t, store, to.ID) != recordSchema {
		t.Fatal("recovery kept the stale lease or its schema", err)
	}
}

func TestCursorRecoveryWaitsForARunningLease(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	armedLease(t, store, to.ID, key)
	if grant, err := claimCursorRecovery(store, to.ID); err != nil || grant.Capability != "" {
		t.Fatal("recovery rotated the credential of a running listener", err)
	}
	if _, err := store.authenticate(to.ID, key); err != nil {
		t.Fatal(err)
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
	claim, err := claimCursorRecovery(store, to.ID)
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
