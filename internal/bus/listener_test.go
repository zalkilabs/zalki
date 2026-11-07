package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// listenSocket binds a private Unix socket so leases can arm a real endpoint.
func listenSocket(t *testing.T) *model.Endpoint {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zalki-lease-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "inbox.sock"), Net: "unix"})
	if errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES) {
		t.Skipf("execution sandbox forbids binding a Unix socket: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	endpoint, err := InspectEndpoint(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func acquire(t *testing.T, store Store, actor, key string) *Lease {
	t.Helper()
	lease, err := store.AcquireListenerLease(actor, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Close(nil) })
	return lease
}

func armedLease(t *testing.T, store Store, actor, key string) *Lease {
	t.Helper()
	lease := acquire(t, store, actor, key)
	if err := lease.Arm(listenSocket(t)); err != nil {
		t.Fatal(err)
	}
	return lease
}

func recordSchemaOf(t *testing.T, store Store, id string) int {
	t.Helper()
	var record struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(sessionBytes(t, store, id), &record); err != nil {
		t.Fatal(err)
	}
	return record.Schema
}

func pendingMessage(t *testing.T, store Store, to Session) Message {
	t.Helper()
	from, key := register(t, store, "codex")
	msg, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(msg.ID, Receipt{Status: StatusPendingActivity}); err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestListenerLeaseRequiresTheRegistrationsOwnCapability(t *testing.T) {
	store := fixtureStore(t)
	from, key, to, _ := registerPair(t, store, "codex", "cursor")
	if _, err := store.AcquireListenerLease(to.ID, "wrong capability"); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("listener accepted a different session capability", err)
	}
	if _, err := store.AcquireListenerLease(from.ID, key); ErrorCodeOf(err) != CodeInvalidInput {
		t.Fatal("listener accepted an identity without a native listener", err)
	}
}

func TestListenerLeaseIsExclusiveUntilClosed(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	first := acquire(t, store, to.ID, key)
	if _, err := store.AcquireListenerLease(to.ID, key); ErrorCodeOf(err) != CodeConflict || !strings.Contains(err.Error(), "already has a listener") {
		t.Fatal("a second lease was acquired while the first held the lifetime lock", err)
	}
	first.Close(nil)
	first.Close(nil) // Idempotent.
	acquire(t, store, to.ID, key)
}

func TestAcquireDropsALockUnlinkedBeforeFlock(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	unlinked := false
	open := func(path string, create bool) (int, error) {
		fd, err := journal.OpenLockFile(path, create)
		if err != nil {
			return fd, err
		}
		if !unlinked {
			if err := os.Remove(path); err != nil {
				_ = journal.CloseLockFile(fd)
				return -1, err
			}
			unlinked = true
		}
		return fd, nil
	}
	fd, err := acquireListenerLock(store.listenerLockPath(to.Kind, to.ID), open)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.CloseLockFile(fd) })
	if !unlinked {
		t.Fatal("lock file was not unlinked between open and flock")
	}
	if second, err := store.AcquireListenerLease(to.ID, key); ErrorCodeOf(err) != CodeConflict {
		if second != nil {
			second.Close(nil)
		}
		t.Fatal("a second lease acquired the replacement lock file", err)
	}
}

func TestListenerLeaseLifecycleWritesLeaseOnlyUnderItsSchema(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	if recordSchemaOf(t, store, to.ID) != recordSchema {
		t.Fatal("an unleased session was not written for older builds")
	}
	lease := armedLease(t, store, to.ID, key)
	current, err := store.session(to.ID)
	if err != nil || current.Listener == nil || current.Listener.Generation != to.Generation || !current.Receive.IdleWake || store.listenerStatus(current) != model.ListenerRunning {
		t.Fatalf("armed lease was not recorded as the running listener: %+v %v", current, err)
	}
	if recordSchemaOf(t, store, to.ID) != leaseSessionSchema {
		t.Fatal("a leased session was readable by builds that would drop its lease")
	}
	if err := lease.Confirm(func(model.SessionView) (json.RawMessage, error) {
		return encodedCursorState(CursorListenerState{RecoverySuppressed: true}), nil
	}); err != nil {
		t.Fatal(err)
	}
	if current, _ := store.session(to.ID); !cursorState(current).RecoverySuppressed {
		t.Fatal("confirmation did not apply the adapter's state")
	}
	msg := pendingMessage(t, store, to)
	if acknowledged, err := lease.ValidateDelivery(msg.ID); err != nil || acknowledged {
		t.Fatal("armed lease rejected its own pending message", err)
	}
	lease.Close(nil)
	current, err = store.session(to.ID)
	if err != nil || current.Listener != nil || current.Receive.IdleWake || store.listenerStatus(current) != model.ListenerNone || recordSchemaOf(t, store, to.ID) != recordSchema {
		t.Fatalf("closing the lease left listener state or the lease schema: %+v %v", current, err)
	}
}

// Required acceptance: a capability rotated after acquisition cannot arm, even
// though the registration generation is unchanged.
func TestArmRejectsACredentialRotatedAfterAcquisition(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	lease := acquire(t, store, to.ID, key)
	grant, err := claimCursorRecovery(store, to.ID)
	if err != nil || grant.Capability == "" {
		t.Fatal("an unarmed lease blocked recovery", err)
	}
	before := sessionBytes(t, store, to.ID)
	if err := lease.Arm(listenSocket(t)); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("a rotated capability armed a listener", err)
	}
	if current, _ := store.session(to.ID); current.Generation != to.Generation || !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
		t.Fatal("a rejected arm changed the record")
	}
}

// Required acceptance: a replacement registration cannot be armed by a lease
// acquired for the generation it replaced.
func TestArmRejectsAReplacementRegistration(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	lease := acquire(t, store, to.ID, key)
	replacement, _, err := store.Register("cursor", NativeID(to), to.CWD, nil)
	if err != nil || replacement.Generation == to.Generation {
		t.Fatal("re-registration kept the generation", err)
	}
	before := sessionBytes(t, store, to.ID)
	if err := lease.Arm(listenSocket(t)); err == nil {
		t.Fatal("a previous generation's lease armed the replacement")
	}
	if !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
		t.Fatal("a rejected arm changed the record")
	}
}

// Same generation, distinct leases: a lease whose process lost its lifetime
// lock without closing (a crash) is fenced by lease identity alone, even when
// its successor armed the very same endpoint.
func TestSupersededLeaseCannotConfirmValidateOrClear(t *testing.T) {
	store := fixtureStore(t)
	to, key := register(t, store, "cursor")
	endpoint := listenSocket(t)
	old := acquire(t, store, to.ID, key)
	if err := old.Arm(endpoint); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(old.fd, syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	current := acquire(t, store, to.ID, key)
	if err := current.Arm(endpoint); err != nil {
		t.Fatal(err)
	}
	msg := pendingMessage(t, store, to)
	before := sessionBytes(t, store, to.ID)
	if err := old.Confirm(func(model.SessionView) (json.RawMessage, error) { return nil, nil }); ErrorCodeOf(err) != CodeConflict {
		t.Fatal("a superseded lease confirmed setup", err)
	}
	if _, err := old.ValidateDelivery(msg.ID); err == nil {
		t.Fatal("a superseded lease validated a delivery")
	}
	old.Close(nil)
	if !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
		t.Fatal("a superseded lease changed the newer lease's record")
	}
	if _, err := current.ValidateDelivery(msg.ID); err != nil {
		t.Fatal("the current lease lost its delivery after the superseded one closed", err)
	}
}

func TestValidateDeliveryKeepsRecipientFences(t *testing.T) {
	store := fixtureStore(t)
	to, key, other, _ := registerPair(t, store, "cursor", "cursor")
	lease := armedLease(t, store, to.ID, key)
	msg := pendingMessage(t, store, to)
	if _, err := lease.ValidateDelivery(pendingMessage(t, store, other).ID); err == nil {
		t.Fatal("validated another recipient's message")
	}
	if _, err := lease.ValidateDelivery(newID()); err == nil {
		t.Fatal("validated an unknown message")
	}
	// Same target generation, different recipient: the recipient check alone.
	misaddressed, err := store.message(pendingMessage(t, store, other).ID)
	if err != nil {
		t.Fatal(err)
	}
	misaddressed.TargetGeneration = to.Generation
	if err := writeJSON(filepath.Join(store.Dir, "messages", misaddressed.ID+".json"), misaddressed); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.ValidateDelivery(misaddressed.ID); err == nil {
		t.Fatal("validated a message addressed to another recipient")
	}
	if _, err := store.Ack(to.ID, key, msg.ID); err != nil {
		t.Fatal(err)
	}
	if acknowledged, err := lease.ValidateDelivery(msg.ID); err != nil || !acknowledged {
		t.Fatal("an acknowledged message was not reported as acknowledged", err)
	}
	waiting := pendingMessage(t, store, to)
	for _, state := range []SessionState{StateApproval, StateRetired} {
		if err := store.Update(to.ID, state, state == StateRetired); err != nil {
			t.Fatal(err)
		}
		if _, err := lease.ValidateDelivery(waiting.ID); err == nil {
			t.Fatalf("validated a delivery for a %s recipient", state)
		}
	}
}

func TestListenerStatusRequiresEndpointAndLifetimeLock(t *testing.T) {
	store := fixtureStore(t)
	to, _ := register(t, store, "cursor")
	if store.listenerStatus(to) != model.ListenerNone {
		t.Fatal("a fresh registration reported a listener")
	}
	fd, err := syscall.Open(store.listenerLockPath(to.Kind, to.ID), syscall.O_CREAT|syscall.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if store.listenerStatus(to) != model.ListenerNone {
		t.Fatal("an unlocked lock file was treated as a listener")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if store.listenerStatus(to) != model.ListenerUnleased {
		t.Fatal("a lock holder without a lease was not reported as unleased")
	}
	to.Listener = &model.ListenerLease{ID: newID(), Generation: to.Generation, Endpoint: model.Endpoint{Path: "/tmp/zalki-missing/inbox.sock"}}
	if store.listenerStatus(to) != model.ListenerStale {
		t.Fatal("a lease without its bound endpoint was reported as running")
	}
	if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	to.Listener.Endpoint = *listenSocket(t)
	if store.listenerStatus(to) != model.ListenerStale {
		t.Fatal("a bound socket without the lifetime lock was reported as running")
	}
}

func TestCursorPendingDoesNotReplayPreviousGeneration(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	from, key, to, _ := registerPair(t, store, "codex", "cursor")

	if _, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "previous registration"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register("cursor", strings.TrimPrefix(to.ID, "cursor:"), to.CWD, nil); err != nil {
		t.Fatal(err)
	}
	if text, err := store.EmitPending(to.ID); err != nil || text != "" {
		t.Fatalf("replayed a message into a replacement registration: %q %v", text, err)
	}
}

// Each recorded ownership field fences every journal operation on its own: a
// record that differs from the lease in only one of them is left unchanged,
// and Close still releases the lease's own lifetime lock.
func TestLeaseOwnershipRequiresEveryRecordedField(t *testing.T) {
	for name, tamper := range map[string]func(*Session){
		"lease ID":                func(v *Session) { v.Listener.ID = newID() },
		"endpoint":                func(v *Session) { v.Listener.Endpoint.Inode++ },
		"lease generation":        func(v *Session) { v.Listener.Generation = newID() },
		"registration generation": func(v *Session) { v.Generation = newID() },
		"registration and lease generation": func(v *Session) {
			v.Generation = newID()
			v.Listener.Generation = v.Generation
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := fixtureStore(t)
			to, key := register(t, store, "cursor")
			msg := pendingMessage(t, store, to)
			lease := armedLease(t, store, to.ID, key)
			if err := store.locked(func() error {
				current, err := store.session(to.ID)
				if err != nil {
					return err
				}
				tamper(&current)
				return store.saveSession(current)
			}); err != nil {
				t.Fatal(err)
			}
			before := sessionBytes(t, store, to.ID)
			if err := lease.Confirm(func(model.SessionView) (json.RawMessage, error) {
				return encodedCursorState(CursorListenerState{RecoverySuppressed: true}), nil
			}); ErrorCodeOf(err) != CodeConflict {
				t.Fatal("confirmed a record owned by a different lease", err)
			}
			if _, err := lease.ValidateDelivery(msg.ID); err == nil {
				t.Fatal("validated a delivery for a record owned by a different lease")
			}
			lease.Close(nil)
			if !bytes.Equal(before, sessionBytes(t, store, to.ID)) {
				t.Fatal("an operation changed a record owned by a different lease")
			}
			if next, err := store.AcquireListenerLease(to.ID, key); err != nil {
				t.Fatal("close did not release the lease's own lifetime lock", err)
			} else {
				next.Close(nil)
			}
		})
	}
}
