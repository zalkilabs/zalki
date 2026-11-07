package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// forwardingHost returns a real bound host, a supplied target and the session
// that must stay unchanged. Both same-actor and cross-actor calls use this setup.
func forwardingHost(t *testing.T, kind string, crossActor bool) (hookRuntime, Session, Session, string) {
	t.Helper()
	store := fixtureStore(t)
	actor, actorKey := register(t, store, kind)
	other, otherKey := register(t, store, kind)
	host := hookRuntime{store: store, actor: actor.ID, kind: kind, event: "PostToolUse"}
	if crossActor {
		return host, other, actor, otherKey
	}
	return host, actor, other, actorKey
}

func forwardingMessage(t *testing.T, store Store, target Session, status DeliveryStatus) Message {
	t.Helper()
	sender, key := register(t, store, "codex")
	msg, _, fresh, err := store.ReserveMessage(SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "context for " + target.ID})
	if err != nil || !fresh {
		t.Fatal("could not reserve forwarding fixture", err)
	}
	msg, err = store.Finish(msg.ID, NewReceipt(status))
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// assertForwardingChanges checks all records, including credentials, leases,
// flood state and unrelated messages. Only the named records may change.
func assertForwardingChanges(t *testing.T, store Store, before map[string]string, changed ...string) {
	t.Helper()
	after := journalSnapshot(t, store)
	for _, path := range changed {
		rel, err := filepath.Rel(store.Dir, path)
		if err != nil {
			t.Fatal(err)
		}
		old, hadOld := before[rel]
		next, hasNext := after[rel]
		if !hadOld || !hasNext || old == next {
			t.Fatalf("expected existing record %s to change", rel)
		}
		delete(before, rel)
		delete(after, rel)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("host call changed records outside the supplied target")
	}
}

// Characterized, not endorsed: UpdateSession forwards the supplied actor, even
// when it differs from the actor bound to this trusted compiled-adapter host.
func TestHookHostUpdateSessionForwardsSuppliedActor(t *testing.T) {
	for _, crossActor := range []bool{false, true} {
		t.Run(forwardingCase(crossActor), func(t *testing.T) {
			host, target, _, _ := forwardingHost(t, "cursor", crossActor)
			original, err := host.store.session(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := journalSnapshot(t, host.store)
			if err := host.UpdateSession(target.ID, StateBusy, false); err != nil {
				t.Fatal(err)
			}
			current, err := host.store.session(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			original.State, original.UpdatedAt = StateBusy, current.UpdatedAt
			if !reflect.DeepEqual(current, original) {
				t.Fatal("host changed more than the supplied session's state and timestamp")
			}
			path, err := host.store.sessionPath(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			assertForwardingChanges(t, host.store, before, path)
		})
	}
}

// Characterized, not endorsed: the forwarded Store operation validates the
// transition before identity. No host actor check takes precedence today.
func TestHookHostUpdateSessionForwardsErrors(t *testing.T) {
	host, target, _, _ := forwardingHost(t, "cursor", true)
	for _, test := range []struct {
		name, id string
		state    SessionState
		want     string
	}{
		{"cross actor invalid transition", target.ID, StateRetired, "invalid_transition: invalid session state/retirement transition"},
		{"transition before malformed identity", "not an actor", StateRetired, "invalid_transition: invalid session state/retirement transition"},
		{"malformed identity", "not an actor", StateBusy, "invalid_identity: use the complete kind:UUID returned by list_agents"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := journalSnapshot(t, host.store)
			if err := host.UpdateSession(test.id, test.state, false); err == nil || err.Error() != test.want {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
			assertForwardingChanges(t, host.store, before)
		})
	}
	before := journalSnapshot(t, host.store)
	if err := host.UpdateSession("cursor:"+newID(), StateBusy, false); err != nil {
		t.Fatal("unknown supplied session should remain a no-op", err)
	}
	assertForwardingChanges(t, host.store, before)
}

// Characterized, not endorsed: EmitPending can emit another actor's context.
// It still emits once, changes only that message, and never acknowledges it.
func TestHookHostEmitPendingForwardsSuppliedActor(t *testing.T) {
	for _, crossActor := range []bool{false, true} {
		t.Run(forwardingCase(crossActor), func(t *testing.T) {
			host, target, other, _ := forwardingHost(t, "cursor", crossActor)
			msg := forwardingMessage(t, host.store, target, StatusPendingActivity)
			forwardingMessage(t, host.store, other, StatusPendingActivity)
			before := journalSnapshot(t, host.store)
			text, err := host.EmitPending(target.ID)
			if err != nil || text != Envelope(msg)+"\n\n" {
				t.Fatalf("context=%q error=%v, want only supplied actor's envelope", text, err)
			}
			current, err := host.store.message(msg.ID)
			if err != nil || current.Status != StatusContextEmitted || current.AcknowledgedAt != "" {
				t.Fatal("emission changed acknowledgement or lost its status", err)
			}
			assertForwardingChanges(t, host.store, before, filepath.Join(host.store.Dir, "messages", msg.ID+".json"))
			before = journalSnapshot(t, host.store)
			if text, err := host.EmitPending(target.ID); err != nil || text != "" {
				t.Fatalf("second emission=%q error=%v, want empty context", text, err)
			}
			assertForwardingChanges(t, host.store, before)
		})
	}
}

// Characterized, not endorsed: EmitPending forwards lookup, capability and
// record errors rather than rejecting the other actor at the host boundary.
func TestHookHostEmitPendingForwardsErrors(t *testing.T) {
	host, target, _, _ := forwardingHost(t, "cursor", true)
	native, _ := register(t, host.store, "codex")
	before := journalSnapshot(t, host.store)
	if text, err := host.EmitPending(native.ID); text != "" || err == nil || err.Error() != "this adapter does not support hook delivery" {
		t.Fatalf("context=%q error=%v, want supplied actor's capability error", text, err)
	}
	assertForwardingChanges(t, host.store, before)
	before = journalSnapshot(t, host.store)
	if text, err := host.EmitPending("cursor:" + newID()); text != "" || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("context=%q error=%v, want supplied actor's missing record", text, err)
	}
	assertForwardingChanges(t, host.store, before)
	msg := forwardingMessage(t, host.store, target, StatusPendingActivity)
	corruptForwardingMessage(t, host.store, msg.ID)
	before = journalSnapshot(t, host.store)
	text, err := host.EmitPending(target.ID)
	var syntax *json.SyntaxError
	if text != "" || !errors.As(err, &syntax) {
		t.Fatalf("context=%q error=%v, want malformed record error", text, err)
	}
	assertForwardingChanges(t, host.store, before)
}

// Characterized, not endorsed: CompletedNativePrompt uses the supplied actor
// to inspect a completed envelope, including one addressed to another actor.
func TestHookHostCompletedNativePromptForwardsSuppliedActor(t *testing.T) {
	for _, crossActor := range []bool{false, true} {
		t.Run(forwardingCase(crossActor), func(t *testing.T) {
			host, target, other, key := forwardingHost(t, "codex", crossActor)
			host.event = "UserPromptSubmit"
			msg := forwardingMessage(t, host.store, target, StatusSubmitted)
			if _, err := host.store.Ack(target.ID, key, msg.ID); err != nil {
				t.Fatal(err)
			}
			before := journalSnapshot(t, host.store)
			prompt := Envelope(msg)
			reason, err := host.CompletedNativePrompt(target.ID, prompt)
			if err != nil || reason != "Zalki queued message already acknowledged; no additional model turn is needed." {
				t.Fatalf("reason=%q error=%v, want supplied actor's completed prompt", reason, err)
			}
			if reason, err := host.CompletedNativePrompt(other.ID, prompt); err != nil || reason != "" {
				t.Fatalf("another recipient matched the envelope: %q %v", reason, err)
			}
			if reason, err := host.CompletedNativePrompt(target.ID, prompt+" changed"); err != nil || reason != "" {
				t.Fatalf("altered prompt matched the envelope: %q %v", reason, err)
			}
			assertForwardingChanges(t, host.store, before)
		})
	}
}

// Characterized, not endorsed: prompt framing is checked before any session
// identity, and a recognized envelope forwards its journal read error.
func TestHookHostCompletedNativePromptForwardsErrors(t *testing.T) {
	host, target, _, _ := forwardingHost(t, "codex", true)
	before := journalSnapshot(t, host.store)
	if reason, err := host.CompletedNativePrompt("not an actor", "ordinary prompt"); err != nil || reason != "" {
		t.Fatalf("ordinary prompt checked host identity: %q %v", reason, err)
	}
	if reason, err := host.CompletedNativePrompt(target.ID, "[zalki message "+newID()+"]\nmissing message"); err != nil || reason != "" {
		t.Fatalf("missing message should be ignored: %q %v", reason, err)
	}
	assertForwardingChanges(t, host.store, before)
	msg := forwardingMessage(t, host.store, target, StatusSubmitted)
	corruptForwardingMessage(t, host.store, msg.ID)
	before = journalSnapshot(t, host.store)
	reason, err := host.CompletedNativePrompt(target.ID, Envelope(msg))
	var syntax *json.SyntaxError
	if reason != "" || !errors.As(err, &syntax) {
		t.Fatalf("reason=%q error=%v, want malformed record error", reason, err)
	}
	assertForwardingChanges(t, host.store, before)
}

func corruptForwardingMessage(t *testing.T, store Store, id string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", id+".json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
}

func forwardingCase(crossActor bool) string {
	if crossActor {
		return "cross actor"
	}
	return "same actor"
}
