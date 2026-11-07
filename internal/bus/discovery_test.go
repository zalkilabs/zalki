package bus

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func TestDiscoverySummarizesEachSessionWithoutRewritingIt(t *testing.T) {
	store := fixtureStore(t)
	for _, test := range []struct {
		kind         string
		listener     bool
		idle         bool
		busy         DeliveryTiming
		experimental bool
	}{
		{"codex", false, true, DeliveryNextTurn, false},
		{"claude", false, true, DeliveryBetweenTools, true},
		{"cursor", false, false, DeliveryNextHook, false},
		{"cursor", true, true, DeliveryNotification, false},
	} {
		v, _ := register(t, store, test.kind)
		v.Name = "fixture reviewer"
		v.State = StateApproval
		// Discovery uses seconds; the full session retains its original precision.
		v.UpdatedAt = "2026-10-01T12:34:56.123456789Z"
		if test.listener {
			v.Listener = &ListenerLease{ID: newID(), Generation: v.Generation}
		}
		if err := store.saveSession(v); err != nil {
			t.Fatal(err)
		}
		path, _ := store.sessionPath(v.ID)
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		items, err := store.Discover()
		if err != nil {
			t.Fatal(err)
		}
		var got *AgentInfo
		for i := range items {
			if items[i].ID == v.ID {
				got = &items[i]
			}
		}
		if got == nil || got.Name != v.Name || got.CWD != v.CWD || got.State != StateApproval || got.UpdatedAt != "2026-10-01T12:34:56Z" {
			t.Fatalf("discovery lost identity/activity: %+v", got)
		}
		if got.IdleWake != nil || (got.Delivery != DeliveryNextHook) != test.idle || got.Delivery != test.busy || got.Experimental != test.experimental {
			t.Fatalf("%s listener=%t: bad summary %+v", test.kind, test.listener, got)
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		var row map[string]json.RawMessage
		wantFields := 6
		if test.experimental {
			wantFields++
		}
		if err := json.Unmarshal(encoded, &row); err != nil || len(row) != wantFields {
			t.Fatalf("unexpected discovery fields: %s %v", encoded, err)
		}
		for _, omitted := range []string{"kind", "receive", "idle_wake", "liveness"} {
			if row[omitted] != nil {
				t.Fatalf("discovery repeated default/detail %s: %s", omitted, encoded)
			}
		}
		after, err := os.ReadFile(path)
		if err != nil || string(after) != string(before) {
			t.Fatal("discovery changed the session record", err)
		}
		full, err := store.List()
		if err != nil {
			t.Fatal(err)
		}
		for _, session := range full {
			if session.ID == v.ID && (session.Receive.Delivery == "" || session.Receive.Inbox == "" || session.UpdatedAt != v.UpdatedAt) {
				t.Fatal("full CLI/smoke session view lost its details")
			}
		}
	}
}

type discoveryAdapter struct {
	stubCodexAdapter
	idle bool
	busy DeliveryTiming
}

func (a discoveryAdapter) ReceiveCapabilities(Session) ReceiveCapabilities {
	c := a.stubCodexAdapter.ReceiveCapabilities(Session{})
	c.IdleWake, c.BusyDelivery = a.idle, a.busy
	return c
}

func TestDiscoveryDefaultsAndWakeOverrides(t *testing.T) {
	for _, timing := range []DeliveryTiming{DeliveryNextTurn, DeliveryBetweenTools, DeliveryNextHook, DeliveryNotification} {
		for _, idle := range []bool{false, true} {
			store := fixtureStore(t)
			registry, err := agent.NewRegistry(discoveryAdapter{idle: idle, busy: timing})
			if err != nil {
				t.Fatal(err)
			}
			store.Registry = registry
			register(t, store, "codex")
			items, err := store.Discover()
			if err != nil || len(items) != 1 {
				t.Fatal("missing registered session", err)
			}
			row := items[0]
			if row.State != "" || row.Liveness != "" || row.Experimental || row.Delivery != timing {
				t.Fatalf("discovery lost defaults/timing: %+v", row)
			}
			wake := row.Delivery != DeliveryNextHook
			if row.IdleWake != nil {
				wake = *row.IdleWake
			}
			if wake != idle || (row.IdleWake != nil) != (idle != (timing != DeliveryNextHook)) {
				t.Fatalf("discovery lost idle capability: idle=%t timing=%s row=%+v", idle, timing, row)
			}
			encoded, _ := json.Marshal(row)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(encoded, &fields); err != nil {
				t.Fatal(err)
			}
			for _, omitted := range []string{"state", "liveness", "experimental", "name"} {
				if fields[omitted] != nil {
					t.Fatalf("default %s must be omitted: %s", omitted, encoded)
				}
			}
		}
	}
}

func TestDiscoveryEmptyList(t *testing.T) {
	items, err := fixtureStore(t).Discover()
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(items)
	if err != nil || string(encoded) != "[]" {
		t.Fatalf("empty discovery must be an array: %s %v", encoded, err)
	}
}
