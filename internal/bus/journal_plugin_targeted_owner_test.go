package bus

import (
	"context"
	"encoding/json"
	"testing"
)

func TestFeedTargetedOwnerBoundsPrecedeNativeRead(t *testing.T) {
	store, session, _, _ := memberCacheFixture(t)
	for _, owner := range []struct {
		scope  string
		status bool
	}{{"none", true}, {"all", false}} {
		work := &memberWorkCount{}
		err := WithJournalWrite(store, func(view JournalView) error {
			raw, err := PluginMembers(t.Context(), view, "first", owner.scope, owner.status, []string{session.ID}, work.charge)
			if err != nil {
				return err
			}
			var result pluginMemberCollection
			if err := json.Unmarshal(raw, &result); err != nil {
				return err
			}
			if len(result.Records) != 0 || work.native != 0 || work.reads != 0 || work.probes != 0 {
				t.Fatal("F_TARGETED_OWNER_BOUNDS_PRECEDE_NATIVE_READ")
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestFeedTargetedMutedProjectionUsesAuthoritativePredicate(t *testing.T) {
	store, active, _, _ := memberCacheFixture(t)
	muted, key := register(t, store, "fixture")
	if err := SetSessionPlugin(store, muted.ID, key, "first", true); err != nil {
		t.Fatal(err)
	}
	result, err := collectMembers(t, store, []string{active.ID, muted.ID}, &memberWorkCount{})
	if err != nil || len(result.Records) != 2 {
		t.Fatal(err)
	}
	for _, row := range result.Records {
		if row.ID == active.ID {
			if row.Class != "active" {
				t.Fatal("F_TARGETED_ACTIVE_PROJECTION_POSITIVE")
			}
			continue
		}
		if row.ID != muted.ID || row.Class != "muted" {
			t.Fatal("F_TARGETED_CLASSIFICATION_USES_PLUGIN_MEMBER")
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(row.Row, &fields); err != nil {
			t.Fatal(err)
		}
		if len(fields) != 3 || fields["id"] == nil || fields["kind"] == nil || string(fields["muted"]) != "true" {
			t.Fatal("F_TARGETED_MUTED_METADATA_STAYS_THREE_FIELDS")
		}
	}
}

func TestFeedTargetedCanceledAndExpiredViewsStayClosed(t *testing.T) {
	store, session, _, _ := memberCacheFixture(t)
	var escaped JournalView
	err := WithJournalWrite(store, func(view JournalView) error {
		escaped = view
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		work := &memberWorkCount{}
		if _, err := PluginMembers(ctx, view, "first", "all", true, []string{session.ID}, work.charge); err == nil || work.reads != 0 || work.native != 0 || work.probes != 0 {
			t.Fatal("F_TARGETED_CANCELED_CONTEXT_BEFORE_NATIVE_READ")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PluginMembers(t.Context(), escaped, "first", "all", true, []string{session.ID}, (&memberWorkCount{}).charge); err == nil {
		t.Fatal("F_TARGETED_EXPIRED_VIEW_HAS_NO_PROJECTION")
	}
	err = WithJournalRead(store, func(view JournalView) error {
		if _, err := PluginMembers(t.Context(), view, "first", "all", true, []string{session.ID}, (&memberWorkCount{}).charge); err == nil {
			t.Fatal("F_TARGETED_READONLY_VIEW_HAS_NO_PROJECTION")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
