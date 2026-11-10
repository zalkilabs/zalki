package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// forwardingHost returns a real bound host, a supplied target and the session
// that must stay unchanged. Both same-actor and cross-actor calls use this setup.
func forwardingHost(t *testing.T, kind string, crossActor bool) (hookRuntime, model.Session, model.Session, string) {
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

func forwardingMessage(t *testing.T, store Store, target model.Session, status model.DeliveryStatus) model.Message {
	t.Helper()
	sender, key := register(t, store, "codex")
	msg, _, fresh, err := store.ReserveMessage(SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "context for " + target.ID})
	if err != nil || !fresh {
		t.Fatal("could not reserve forwarding fixture", err)
	}
	msg, err = store.Finish(msg.ID, model.NewReceipt(status))
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

// Item 0 characterized cross-actor forwarding. The host now refuses it while
// preserving the matching actor's existing state update behavior.
func TestHookHostUpdateSessionUsesBoundActor(t *testing.T) {
	for _, crossActor := range []bool{false, true} {
		t.Run(forwardingCase(crossActor), func(t *testing.T) {
			host, target, _, _ := forwardingHost(t, "cursor", crossActor)
			if crossActor {
				seedBindingLease(t, host.store, target)
				forwardingMessage(t, host.store, target, model.StatusPendingActivity)
				before := journalSnapshot(t, host.store)
				assertActorRefusal(t, host.UpdateSession(target.ID, model.StateBusy, false))
				assertActorRefusal(t, host.UpdateSession(target.ID, model.StateRetired, true))
				assertForwardingChanges(t, host.store, before)
				return
			}
			original, err := host.store.session(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			before := journalSnapshot(t, host.store)
			if err := host.UpdateSession(target.ID, model.StateBusy, false); err != nil {
				t.Fatal(err)
			}
			current, err := host.store.session(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			original.State, original.UpdatedAt = model.StateBusy, current.UpdatedAt
			if !reflect.DeepEqual(current, original) {
				t.Fatal("host changed more than the bound session's state and timestamp")
			}
			path, err := host.store.sessionPath(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			allowBoundStateEvent(t, host.store, target, before)
			assertForwardingChanges(t, host.store, before, path)
		})
	}
}

// Binding now precedes Store validation. Same-actor errors and the matching
// missing-session no-op remain unchanged from the item-0 characterization.
func TestHookHostUpdateSessionErrorsFollowActorCheck(t *testing.T) {
	host, target, other, _ := forwardingHost(t, "cursor", false)
	for _, test := range []struct {
		name, id string
	}{
		{"other actor and invalid transition", other.ID},
		{"malformed identity and invalid transition", "not an actor"},
	} {
		t.Run(test.name, func(t *testing.T) {
			before := journalSnapshot(t, host.store)
			assertActorRefusal(t, host.UpdateSession(test.id, model.StateRetired, false))
			assertForwardingChanges(t, host.store, before)
		})
	}
	before := journalSnapshot(t, host.store)
	assertActorRefusal(t, host.UpdateSession("not an actor", model.StateBusy, false))
	err := host.UpdateSession(target.ID, model.StateRetired, false)
	if model.ErrorCodeOf(err) != model.CodeInvalidTransition || err.Error() != "invalid_transition: invalid session state/retirement transition" {
		t.Fatalf("same-actor error=%v, want unchanged invalid transition", err)
	}
	missing := "cursor:" + newID()
	assertActorRefusal(t, host.UpdateSession(missing, model.StateBusy, false))
	host.actor = missing
	if err := host.UpdateSession(missing, model.StateBusy, false); err != nil {
		t.Fatal("matching unknown session should remain a no-op", err)
	}
	assertForwardingChanges(t, host.store, before)
}

// Another actor's context must remain private and pending. A matching host
// still emits its own envelope once, without acknowledging it.
func TestHookHostEmitPendingUsesBoundActor(t *testing.T) {
	for _, crossActor := range []bool{false, true} {
		t.Run(forwardingCase(crossActor), func(t *testing.T) {
			host, target, other, _ := forwardingHost(t, "cursor", crossActor)
			msg := forwardingMessage(t, host.store, target, model.StatusPendingActivity)
			forwardingMessage(t, host.store, other, model.StatusPendingActivity)
			before := journalSnapshot(t, host.store)
			if crossActor {
				assertStringActorRefusal(t, host.EmitPending, target.ID)
				assertStringActorRefusal(t, host.EmitPending, target.ID)
				assertForwardingChanges(t, host.store, before)
				return
			}
			text, err := host.EmitPending(target.ID)
			if err != nil || text != model.Envelope(msg)+"\n\n" {
				t.Fatalf("context=%q error=%v, want bound actor's envelope", text, err)
			}
			current, err := host.store.message(msg.ID)
			if err != nil || current.Status != model.StatusContextEmitted || current.AcknowledgedAt != "" {
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

// Binding wins over another actor's lookup, capability or malformed-record
// error; matching actors retain those existing downstream errors.
func TestHookHostEmitPendingErrorsFollowActorCheck(t *testing.T) {
	host, target, other, _ := forwardingHost(t, "cursor", false)
	native, _ := register(t, host.store, "codex")
	before := journalSnapshot(t, host.store)
	assertStringActorRefusal(t, host.EmitPending, native.ID)
	nativeHost := hookRuntime{store: host.store, actor: native.ID, kind: native.Kind}
	if text, err := nativeHost.EmitPending(native.ID); text != "" || err == nil || err.Error() != "this adapter does not support hook delivery" {
		t.Fatalf("context=%q error=%v, want bound actor's capability error", text, err)
	}
	missing := "cursor:" + newID()
	assertStringActorRefusal(t, host.EmitPending, missing)
	missingHost := host
	missingHost.actor = missing
	if text, err := missingHost.EmitPending(missing); text != "" || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("context=%q error=%v, want bound actor's missing record", text, err)
	}
	assertForwardingChanges(t, host.store, before)
	// One malformed message file no longer fails hook delivery (it is skipped
	// and reported), so the bound actor's own malformed session record is the
	// error that must surface here.
	forwardingMessage(t, host.store, target, model.StatusPendingActivity)
	if err := os.WriteFile(host.store.sessionPathMust(t, target.ID), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	before = journalSnapshot(t, host.store)
	wrongHost := host
	wrongHost.actor = other.ID
	assertStringActorRefusal(t, wrongHost.EmitPending, target.ID)
	text, err := host.EmitPending(target.ID)
	var syntax *json.SyntaxError
	if text != "" || !errors.As(err, &syntax) {
		t.Fatalf("context=%q error=%v, want bound actor's malformed record error", text, err)
	}
	assertForwardingChanges(t, host.store, before)
}

// Completed envelope evidence belongs to the bound actor. The original exact
// reason and read-only behavior remain for a matching actor.
func TestHookHostCompletedNativePromptUsesBoundActor(t *testing.T) {
	for _, crossActor := range []bool{false, true} {
		t.Run(forwardingCase(crossActor), func(t *testing.T) {
			host, target, other, key := forwardingHost(t, "codex", crossActor)
			host.event = "UserPromptSubmit"
			msg := forwardingMessage(t, host.store, target, model.StatusSubmitted)
			if _, err := host.store.Ack(target.ID, key, msg.ID); err != nil {
				t.Fatal(err)
			}
			before := journalSnapshot(t, host.store)
			prompt := model.Envelope(msg)
			if crossActor {
				assertPromptActorRefusal(t, host, target.ID, prompt)
				assertPromptActorRefusal(t, host, target.ID, prompt+" changed")
				assertPromptResult(t, host, other.ID, prompt, "")
			} else {
				assertPromptResult(t, host, target.ID, prompt, "Zalki queued message already acknowledged; no additional model turn is needed.")
				assertPromptActorRefusal(t, host, other.ID, prompt)
				assertPromptResult(t, host, target.ID, prompt+" changed", "")
			}
			assertForwardingChanges(t, host.store, before)
		})
	}
}

// Binding now precedes prompt framing and journal errors. Ignored prompts,
// missing messages and malformed records retain their same-actor behavior.
func TestHookHostCompletedNativePromptErrorsFollowActorCheck(t *testing.T) {
	host, target, other, _ := forwardingHost(t, "codex", false)
	before := journalSnapshot(t, host.store)
	for _, prompt := range []string{"ordinary prompt", "[zalki message not-a-uuid"} {
		assertPromptActorRefusal(t, host, other.ID, prompt)
		if reason, err := host.CompletedNativePrompt(target.ID, prompt); err != nil || reason != "" {
			t.Fatalf("bound actor's ignored prompt=%q error=%v", reason, err)
		}
	}
	assertPromptActorRefusal(t, host, "not an actor", "ordinary prompt")
	missingPrompt := "[zalki message " + newID() + "]\nmissing message"
	assertPromptActorRefusal(t, host, other.ID, missingPrompt)
	if reason, err := host.CompletedNativePrompt(target.ID, missingPrompt); err != nil || reason != "" {
		t.Fatalf("missing message should be ignored for bound actor: %q %v", reason, err)
	}
	assertForwardingChanges(t, host.store, before)
	msg := forwardingMessage(t, host.store, target, model.StatusSubmitted)
	corruptForwardingMessage(t, host.store, msg.ID)
	before = journalSnapshot(t, host.store)
	wrongHost := host
	wrongHost.actor = other.ID
	assertPromptActorRefusal(t, wrongHost, target.ID, model.Envelope(msg))
	reason, err := host.CompletedNativePrompt(target.ID, model.Envelope(msg))
	var syntax *json.SyntaxError
	if reason != "" || !errors.As(err, &syntax) {
		t.Fatalf("reason=%q error=%v, want bound actor's malformed record error", reason, err)
	}
	assertForwardingChanges(t, host.store, before)
}

func assertActorRefusal(t *testing.T, err error) {
	t.Helper()
	const want = "unauthorized: hook operation must use its bound session"
	if err == nil || model.ErrorCodeOf(err) != model.CodeUnauthorized || err.Error() != want {
		t.Fatalf("error=%v, want code unauthorized and exact text %q", err, want)
	}
}

func assertStringActorRefusal(t *testing.T, call func(string) (string, error), id string) {
	t.Helper()
	text, err := call(id)
	assertActorRefusal(t, err)
	if text != "" {
		t.Fatal("refusal returned context")
	}
}

func assertPromptActorRefusal(t *testing.T, host hookRuntime, id, prompt string) {
	t.Helper()
	text, err := host.CompletedNativePrompt(id, prompt)
	assertActorRefusal(t, err)
	if text != "" {
		t.Fatal("refusal returned a completion reason")
	}
}

func assertPromptResult(t *testing.T, host hookRuntime, id, prompt, want string) {
	t.Helper()
	reason, err := host.CompletedNativePrompt(id, prompt)
	if err != nil || reason != want {
		t.Fatalf("reason=%q error=%v, want %q and no error", reason, err, want)
	}
}

type bindingOperation struct {
	name string
	call func(hookRuntime, string) (string, error)
}

func bindingOperations(prompt string) []bindingOperation {
	return []bindingOperation{
		{"UpdateSession", func(h hookRuntime, id string) (string, error) { return "", h.UpdateSession(id, model.StateBusy, false) }},
		{"EmitPending", func(h hookRuntime, id string) (string, error) { return h.EmitPending(id) }},
		{"CompletedNativePrompt", func(h hookRuntime, id string) (string, error) { return h.CompletedNativePrompt(id, prompt) }},
	}
}

func TestHookHostRejectsUnboundOrDifferentActor(t *testing.T) {
	host, _, other, _ := forwardingHost(t, "cursor", false)
	for _, operation := range bindingOperations("ordinary prompt") {
		t.Run(operation.name, func(t *testing.T) {
			for _, test := range []struct {
				name, actor, id string
				zeroStore       bool
			}{
				{"empty_binding_empty_id", "", "", true},
				{"empty_binding_registered_id", "", other.ID, false},
				{"empty_id", host.actor, "", false},
				{"different_actor", host.actor, other.ID, false},
				{"different_kind", host.actor, "codex:" + strings.TrimPrefix(host.actor, "cursor:"), false},
				{"noncanonical_spelling", host.actor, strings.ToUpper(host.actor), false},
			} {
				t.Run(test.name, func(t *testing.T) {
					before := journalSnapshot(t, host.store)
					candidate := host
					candidate.actor = test.actor
					if test.zeroStore {
						candidate = hookRuntime{}
					}
					text, err := operation.call(candidate, test.id)
					assertActorRefusal(t, err)
					if text != "" {
						t.Fatal("refusal returned output")
					}
					assertForwardingChanges(t, host.store, before)
				})
			}
		})
	}
}

func TestHookHostChecksActorBeforeStoreIO(t *testing.T) {
	prompt := "[zalki message 00000000-0000-4000-8000-000000000001]\nmissing message"
	for _, operation := range bindingOperations(prompt) {
		t.Run(operation.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "never-created")
			host := hookRuntime{store: Store{Dir: dir, Registry: testRegistry()}, actor: "cursor:00000000-0000-4000-8000-000000000002"}
			text, err := operation.call(host, "cursor:00000000-0000-4000-8000-000000000003")
			assertActorRefusal(t, err)
			if text != "" {
				t.Fatal("refusal returned output")
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("host entered Store before actor check: %v", err)
			}
		})
	}
}

func TestUnboundHookRuntimeStillSupportsHealth(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "cursor")
	seedBindingLease(t, store, target)
	target, err := store.session(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := journalSnapshot(t, store)
	var health agent.HealthHost = hookRuntime{store: store}
	if got := health.Listener(target); got != model.ListenerStale || got != store.listenerStatus(target) {
		t.Fatalf("unbound health=%s, want unchanged stale-listener evidence", got)
	}
	assertForwardingChanges(t, store, before)
}

func seedBindingLease(t *testing.T, store Store, target model.Session) {
	t.Helper()
	target.Listener = &model.ListenerLease{ID: newID(), Generation: target.Generation, Endpoint: model.Endpoint{Path: filepath.Join(store.Dir, "missing.sock")}}
	if err := store.locked(func() error { return store.saveSession(target) }); err != nil {
		t.Fatal(err)
	}
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
