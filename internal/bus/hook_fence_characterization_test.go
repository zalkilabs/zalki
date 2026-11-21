package bus

import (
	"errors"
	"os"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// A hook captures the registration it started with. Store functions called
// with that capture must refuse a changed or missing registration and must
// treat a call without one as an ordinary session lookup.

const staleGeneration = "00000000-0000-4000-8000-00000000dead"

type fenceKind struct {
	name  string
	stale bool
	make  func(v model.Session) []sessionFence
}

func fenceKinds() []fenceKind {
	bound := func(v model.Session) sessionFence {
		return sessionFence{actor: v.ID, kind: v.Kind, generation: v.Generation, bound: true}
	}
	return []fenceKind{
		{"no capture", false, func(model.Session) []sessionFence { return nil }},
		{"unbound capture", false, func(v model.Session) []sessionFence {
			h := bound(v)
			h.bound = false
			return []sessionFence{h}
		}},
		{"matching capture", false, func(v model.Session) []sessionFence { return []sessionFence{bound(v)} }},
		{"other generation", true, func(v model.Session) []sessionFence {
			h := bound(v)
			h.generation = staleGeneration
			return []sessionFence{h}
		}},
		{"empty generation", true, func(v model.Session) []sessionFence {
			h := bound(v)
			h.generation = ""
			return []sessionFence{h}
		}},
		{"other kind", true, func(v model.Session) []sessionFence {
			h := bound(v)
			h.kind = "codex"
			return []sessionFence{h}
		}},
		{"other actor", true, func(v model.Session) []sessionFence {
			h := bound(v)
			h.actor = "cursor:00000000-0000-4000-8000-0000000000aa"
			return []sessionFence{h}
		}},
	}
}

func TestSessionUnderFenceAppliesTheCapture(t *testing.T) {
	store := fixtureStore(t)
	v, _ := register(t, store, "cursor")
	for _, kind := range fenceKinds() {
		got, err := sessionUnderFence(store, v.ID, kind.make(v)...)
		if kind.stale {
			if !errors.Is(err, errStaleHook) {
				t.Errorf("%s: error = %v, want a stale hook", kind.name, err)
			}
			continue
		}
		if err != nil || got.ID != v.ID || got.Generation != v.Generation {
			t.Errorf("%s: got %q %q, error %v", kind.name, got.ID, got.Generation, err)
		}
	}
}

func TestSessionUnderFenceWithAMissingRecord(t *testing.T) {
	store := fixtureStore(t)
	missing := "cursor:00000000-0000-4000-8000-0000000000bb"
	ghost := model.Session{ID: missing, Kind: "cursor", Generation: staleGeneration}
	for _, kind := range fenceKinds() {
		_, err := sessionUnderFence(store, missing, kind.make(ghost)...)
		// A bound capture never adopts a record that is missing; an unbound
		// one and no capture report the missing record as it is.
		if wantStale := kind.name == "matching capture" || kind.stale; wantStale != errors.Is(err, errStaleHook) {
			t.Errorf("%s: error = %v", kind.name, err)
		}
		if wantMissing := kind.name == "no capture" || kind.name == "unbound capture"; wantMissing != errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: error = %v", kind.name, err)
		}
	}
}

func TestUpdateSessionRefusesAStaleCaptureAndKeepsTheRecord(t *testing.T) {
	for _, kind := range fenceKinds() {
		store := fixtureStore(t)
		v, _ := register(t, store, "cursor")
		before := mustSession(t, store, v.ID)
		err := updateSession(store, v.ID, model.StateBusy, false, model.CauseTool, kind.make(v)...)
		after := mustSession(t, store, v.ID)
		if kind.stale {
			if !errors.Is(err, errStaleHook) || after.State != before.State {
				t.Errorf("%s: error %v, state %q -> %q", kind.name, err, before.State, after.State)
			}
			continue
		}
		if err != nil || after.State != model.StateBusy {
			t.Errorf("%s: error %v, state %q", kind.name, err, after.State)
		}
	}
}

func TestUpdateSessionWithAMissingRecord(t *testing.T) {
	store := fixtureStore(t)
	missing := "cursor:00000000-0000-4000-8000-0000000000bb"
	ghost := model.Session{ID: missing, Kind: "cursor", Generation: staleGeneration}
	if err := updateSession(store, missing, model.StateBusy, false, model.CauseTool); err != nil {
		t.Errorf("no capture: error = %v, want none", err)
	}
	unbound := sessionFence{actor: missing, kind: "cursor", generation: staleGeneration}
	if err := updateSession(store, missing, model.StateBusy, false, model.CauseTool, unbound); err != nil {
		t.Errorf("unbound capture: error = %v, want none", err)
	}
	if err := updateSession(store, missing, model.StateBusy, false, model.CauseTool, fenceKinds()[2].make(ghost)...); !errors.Is(err, errStaleHook) {
		t.Errorf("matching capture of a missing record: error = %v, want a stale hook", err)
	}
}

func TestHookNameRefusesAStaleCaptureAndKeepsTheName(t *testing.T) {
	for _, kind := range fenceKinds() {
		store := fixtureStore(t)
		v, _ := register(t, store, "cursor")
		err := store.hookName(v.ID, "chosen", kind.make(v)...)
		name := mustSession(t, store, v.ID).Name
		if kind.stale {
			if !errors.Is(err, errStaleHook) || name != "" {
				t.Errorf("%s: error %v, name %q", kind.name, err, name)
			}
			continue
		}
		if err != nil || name != "chosen" {
			t.Errorf("%s: error %v, name %q", kind.name, err, name)
		}
	}
}

func TestRememberOwnerRefusesAStaleCaptureAndKeepsTheBuild(t *testing.T) {
	for _, kind := range fenceKinds() {
		store := fixtureStore(t)
		v, _ := register(t, store, "cursor")
		v.HookBuild = "recorded-build"
		if err := store.locked(func() error { return store.saveSession(v) }); err != nil {
			t.Fatal(err)
		}
		err := store.rememberOwner(v.ID, "cursor", false, kind.make(v)...)
		build := mustSession(t, store, v.ID).HookBuild
		if kind.stale {
			if !errors.Is(err, errStaleHook) || build != "recorded-build" {
				t.Errorf("%s: error %v, build %q", kind.name, err, build)
			}
			continue
		}
		if err != nil || build != currentExecutable.loadedBuild {
			t.Errorf("%s: error %v, build %q", kind.name, err, build)
		}
	}
}

func TestReleaseApprovalQueueRefusesAStaleCapture(t *testing.T) {
	for _, kind := range fenceKinds() {
		store := fixtureStore(t)
		v, _ := register(t, store, "cursor")
		err := (&Service{Store: store}).releaseApprovalQueue(t.Context(), v.ID, kind.make(v)...)
		if kind.stale != errors.Is(err, errStaleHook) || (!kind.stale && err != nil) {
			t.Errorf("%s: error = %v", kind.name, err)
		}
	}
}

func TestNotifyWatchesRefusesAStaleCapture(t *testing.T) {
	stop := agent.HookInput{Event: "Stop"}
	for _, kind := range fenceKinds() {
		store := fixtureStore(t)
		v, _ := register(t, store, "cursor")
		err := (&Service{Store: store}).notifyWatches(t.Context(), v.ID, stop, agent.Lifecycle{}, time.Now(), kind.make(v)...)
		if kind.stale != errors.Is(err, errStaleHook) || (!kind.stale && err != nil) {
			t.Errorf("%s: error = %v", kind.name, err)
		}
	}
	// Events that never notify do not look at the capture at all.
	store := fixtureStore(t)
	v, _ := register(t, store, "cursor")
	stale := fenceKinds()[3].make(v)
	if err := (&Service{Store: store}).notifyWatches(t.Context(), v.ID, agent.HookInput{Event: "PostToolUse"}, agent.Lifecycle{}, time.Now(), stale...); err != nil {
		t.Errorf("non-completion event with a stale capture: error = %v, want none", err)
	}
}

func TestConsumeHookWatchesAppliesTheCapture(t *testing.T) {
	for _, kind := range fenceKinds() {
		store := fixtureStore(t)
		v, _ := register(t, store, "cursor")
		due, err := consumeHookWatches(store, v.ID, time.Now(), kind.make(v)...)
		if kind.stale {
			if !errors.Is(err, errStaleHook) {
				t.Errorf("%s: error = %v, want a stale hook", kind.name, err)
			}
			continue
		}
		if err != nil || len(due) != 0 {
			t.Errorf("%s: error %v, %d due watches", kind.name, err, len(due))
		}
	}
	// A capture, even an unbound one, makes a missing record an error here;
	// without a capture the missing target is not looked up at all.
	store := fixtureStore(t)
	missing := "cursor:00000000-0000-4000-8000-0000000000bb"
	if _, err := consumeHookWatches(store, missing, time.Now()); err != nil {
		t.Errorf("no capture, missing record: error = %v, want none", err)
	}
	unbound := sessionFence{actor: missing, kind: "cursor"}
	if _, err := consumeHookWatches(store, missing, time.Now(), unbound); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unbound capture, missing record: error = %v, want a missing record", err)
	}
}

func TestLifecycleSessionForHookAppliesTheCapture(t *testing.T) {
	store := fixtureStore(t)
	v, _ := register(t, store, "cursor")
	for _, kind := range fenceKinds() {
		caps := kind.make(v)
		h := hookRuntime{store: store, actor: v.ID, kind: v.Kind}
		if len(caps) == 1 {
			h = hookRuntime{store: store, actor: caps[0].actor, kind: caps[0].kind, generation: caps[0].generation, generationBound: caps[0].bound}
		}
		got, ok, err := lifecycleSessionForHook(h)
		switch {
		case kind.stale:
			if !errors.Is(err, errStaleHook) || ok {
				t.Errorf("%s: ok %v, error %v, want a stale hook", kind.name, ok, err)
			}
		default:
			if err != nil || !ok || got.ID != v.ID {
				t.Errorf("%s: ok %v, error %v", kind.name, ok, err)
			}
		}
	}
	missing := hookRuntime{store: store, actor: "cursor:00000000-0000-4000-8000-0000000000bb", kind: "cursor", generation: staleGeneration, generationBound: true}
	if _, ok, err := lifecycleSessionForHook(missing); ok || !errors.Is(err, errStaleHook) {
		t.Errorf("bound capture of a missing record: ok %v, error %v, want false and a stale hook", ok, err)
	}
	unboundMissing := hookRuntime{store: store, actor: "cursor:00000000-0000-4000-8000-0000000000bb", kind: "cursor"}
	if _, ok, err := lifecycleSessionForHook(unboundMissing); ok || err != nil {
		t.Errorf("unbound capture of a missing record: ok %v, error %v, want false and none", ok, err)
	}
	wrongKind := hookRuntime{store: store, actor: v.ID, kind: "codex"}
	if _, _, err := lifecycleSessionForHook(wrongKind); model.ErrorCodeOf(err) != model.CodeInvalidIdentity {
		t.Errorf("unbound capture of another agent: error = %v, want an identity error", err)
	}
}

func mustSession(t *testing.T, store Store, id string) model.Session {
	t.Helper()
	v, err := store.session(id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestFenceOfCopiesTheCapturedRegistration(t *testing.T) {
	h := hookRuntime{actor: "cursor:a", kind: "cursor", event: "Stop", generation: "g1", generationBound: true}
	if got, want := fenceOf(h), (sessionFence{actor: "cursor:a", kind: "cursor", generation: "g1", bound: true}); got != want {
		t.Errorf("fenceOf = %+v, want %+v", got, want)
	}
	h.generationBound = false
	if fenceOf(h).bound {
		t.Error("an unbound host gave a bound fence")
	}
}
