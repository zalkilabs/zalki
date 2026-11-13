package bus

import (
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type countedMemberAdapter struct {
	livenessStub
	calls *atomic.Int64
	alive *atomic.Bool
}

func (a countedMemberAdapter) Liveness(agent.LivenessView, agent.LivenessProbes) model.Liveness {
	a.calls.Add(1)
	if a.alive.Load() {
		return model.LivenessAlive
	}
	return model.LivenessExited
}

func memberCacheFixture(t *testing.T) (Store, model.Session, *atomic.Int64, *atomic.Bool) {
	t.Helper()
	calls, alive := &atomic.Int64{}, &atomic.Bool{}
	alive.Store(true)
	registry, err := agent.NewRegistry(countedMemberAdapter{livenessStub: livenessStub{}, calls: calls, alive: alive})
	if err != nil {
		t.Fatal(err)
	}
	store := fixtureStore(t)
	store.Registry = registry
	session, _ := register(t, store, "fixture")
	calls.Store(0)
	return store, session, calls, alive
}

func TestFeedMemberEvidenceIsOncePerViewAndPluginIsolated(t *testing.T) {
	t.Setenv("ZALKI_PLUGINS", "first")
	store, session, calls, alive := memberCacheFixture(t)
	var escaped JournalView
	err := WithJournalRead(store, func(view JournalView) error {
		escaped = view
		for _, name := range []string{"first", "second", "first", "second"} {
			member, err := view.PluginMember(session.ID, name)
			if err != nil || member != (name == "first") {
				t.Fatal("F_CACHE_PLUGIN_CLASSIFICATIONS_ISOLATED", name, err)
			}
		}
		if calls.Load() != 1 {
			t.Fatal("F_CACHE_ONE_NATIVE_PROBE_PER_VIEW", calls.Load())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := escaped.PluginMember(session.ID, "first"); err == nil {
		t.Fatal("F_CACHE_EXPIRED_VIEW_CLOSED")
	}
	if _, ok := pluginMemberViews.Load(store.Dir); ok {
		t.Fatal("F_CACHE_NO_POSITIVE_AFTER_VIEW")
	}
	alive.Store(false)
	err = WithJournalRead(store, func(view JournalView) error {
		member, err := view.PluginMember(session.ID, "first")
		if err != nil || member || calls.Load() != 2 {
			t.Fatal("F_CACHE_NO_POSITIVE_ACROSS_STEPS", member, calls.Load(), err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFeedMemberCacheInvalidatesOnNativeWrite(t *testing.T) {
	store, session, calls, _ := memberCacheFixture(t)
	err := WithJournalWrite(store, func(view JournalView) error {
		if member, err := view.PluginMember(session.ID, "first"); err != nil || !member {
			t.Fatal(err)
		}
		session.State, session.Retired = model.StateRetired, true
		if err := store.saveSession(session); err != nil {
			return err
		}
		if member, err := view.PluginMember(session.ID, "first"); err != nil || member {
			t.Fatal("F_CACHE_NATIVE_WRITE_INVALIDATES", err)
		}
		session.State, session.Retired = model.StateReady, false
		// Carry the prepared audit forward, as ordinary lifecycle writes do.
		current, err := store.session(session.ID)
		if err != nil {
			return err
		}
		session.PluginPolicy = current.PluginPolicy
		if err := store.saveSession(session); err != nil {
			return err
		}
		if member, err := view.PluginMember(session.ID, "first"); err != nil || !member || calls.Load() != 2 {
			t.Fatal("F_CACHE_REACTIVATE_RECHECKS_NATIVE", calls.Load(), err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFeedMemberCacheDoesNotShareEvidenceAcrossRoots(t *testing.T) {
	first, session, firstCalls, _ := memberCacheFixture(t)
	second, other, secondCalls, alive := memberCacheFixture(t)
	firstPath, err := first.sessionPath(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := second.sessionPath(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(secondPath, raw); err != nil {
		t.Fatal(err)
	}
	otherPath, err := second.sessionPath(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(otherPath); err != nil {
		t.Fatal(err)
	}
	alive.Store(false)
	err = WithJournalRead(first, func(view JournalView) error {
		if member, err := view.PluginMember(session.ID, "first"); err != nil || !member {
			t.Fatal(err)
		}
		return WithJournalRead(second, func(otherView JournalView) error {
			member, err := otherView.PluginMember(session.ID, "first")
			if err != nil || member || firstCalls.Load() != 1 || secondCalls.Load() != 1 {
				t.Fatal("F_CACHE_ROOTS_ARE_ISOLATED", err)
			}
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

type decodedMemberAdapter struct {
	countedMemberAdapter
	decodes *atomic.Int64
}

func (a decodedMemberAdapter) DecodeState(in agent.StateInput) (json.RawMessage, error) {
	a.decodes.Add(1)
	return a.livenessStub.DecodeState(in)
}

func TestFeedMemberReadsAndDecodesNativeOncePerView(t *testing.T) {
	calls, decodes, alive := &atomic.Int64{}, &atomic.Int64{}, &atomic.Bool{}
	alive.Store(true)
	adapter := decodedMemberAdapter{countedMemberAdapter: countedMemberAdapter{livenessStub: livenessStub{}, calls: calls, alive: alive}, decodes: decodes}
	registry, err := agent.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	store := fixtureStore(t)
	store.Registry = registry
	session, _ := register(t, store, "fixture")
	calls.Store(0)
	decodes.Store(0)
	if err := WithJournalRead(store, func(view JournalView) error {
		for _, name := range []string{"first", "second", "first", "second"} {
			if member, err := view.PluginMember(session.ID, name); err != nil || !member {
				t.Fatal("member positive", err)
			}
		}
		if calls.Load() != 1 || decodes.Load() != 1 {
			t.Fatal("F_CACHE_ONE_NATIVE_DECODE_AND_PROBE_PER_VIEW", decodes.Load(), calls.Load())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestFeedMemberZeroReferenceCacheCannotBeRevived(t *testing.T) {
	store, session, calls, alive := memberCacheFixture(t)
	var previous *pluginMemberCache
	if err := WithJournalRead(store, func(view JournalView) error {
		if member, err := view.PluginMember(session.ID, "first"); err != nil || !member {
			t.Fatal(err)
		}
		previous = view.scope.members
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if previous == nil || previous.refs.Load() != 0 {
		t.Fatal("expired cache fixture")
	}
	// This is the exact state after the last release decrements to zero but
	// before its CompareAndDelete. Native evidence is now different.
	pluginMemberViews.Store(store.Dir, previous)
	alive.Store(false)
	if err := WithJournalRead(store, func(view JournalView) error {
		member, err := view.PluginMember(session.ID, "first")
		if err != nil || member || view.scope.members == previous || calls.Load() != 2 {
			t.Fatal("F_CACHE_ZERO_REF_RESIDUE_NEVER_REVIVED", member, calls.Load(), err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
