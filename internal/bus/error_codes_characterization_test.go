package bus_test

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// TestCallerVisibleErrorCodes pins the error code each public operation
// reports for the common refusals. Callers (the MCP server and the CLI) branch
// on these codes, so they must not change when the code moves between packages.
func TestCallerVisibleErrorCodes(t *testing.T) {
	const missing = "beta:11111111-1111-4111-8111-111111111111"
	tests := []struct {
		name string
		call func(t *testing.T, store bus.Store, from, to agent) error
		want model.ErrorCode
	}{
		{"send with another session's capability", func(_ *testing.T, store bus.Store, from, to agent) error {
			return send(store, bus.SendRequest{Actor: from.ID, Capability: to.Key, To: to.ID, Body: "hi"})
		}, "unauthorized"},
		{"send to a session that does not exist", func(_ *testing.T, store bus.Store, from, _ agent) error {
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: missing, Body: "hi"})
		}, "not_found"},
		{"send to a retired session", func(t *testing.T, store bus.Store, from, to agent) error {
			t.Helper()
			if err := store.Update(to.ID, model.StateRetired, true); err != nil {
				t.Fatal(err)
			}
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "hi"})
		}, "unavailable"},
		{"send with an explicit message ID that is not a UUID", func(_ *testing.T, store bus.Store, from, to agent) error {
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, MessageID: "invalid", Body: "hi"})
		}, "invalid_identity"},
		{"send an empty body", func(_ *testing.T, store bus.Store, from, to agent) error {
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID})
		}, "invalid_input"},
		{"send a body with NUL", func(_ *testing.T, store bus.Store, from, to agent) error {
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "a\x00b"})
		}, "invalid_input"},
		{"send a body over 16 KiB", func(_ *testing.T, store bus.Store, from, to agent) error {
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: strings.Repeat("x", 16*1024+1)})
		}, "invalid_input"},
		{"send a body with invalid UTF-8", func(_ *testing.T, store bus.Store, from, to agent) error {
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "\xff"})
		}, "invalid_input"},
		{"send the sender's own capability", func(_ *testing.T, store bus.Store, from, to agent) error {
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "key " + from.Key})
		}, "invalid_input"},
		{"retry a message ID with a different body", func(t *testing.T, store bus.Store, from, to agent) error {
			t.Helper()
			id := newUUID(t)
			if err := send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, MessageID: id, Body: "one"}); err != nil {
				t.Fatal(err)
			}
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, MessageID: id, Body: "two"})
		}, "conflict"},
		{"acknowledge as someone other than the recipient", func(t *testing.T, store bus.Store, from, to agent) error {
			t.Helper()
			msg := sendOK(t, store, from, to)
			_, err := store.Ack(from.ID, from.Key, msg.ID)
			return err
		}, "unauthorized"},
		{"acknowledge a message that does not exist", func(_ *testing.T, store bus.Store, _, to agent) error {
			_, err := store.Ack(to.ID, to.Key, "11111111-1111-4111-8111-111111111111")
			return err
		}, "not_found"},
		{"acknowledge with a malformed message ID", func(_ *testing.T, store bus.Store, _, to agent) error {
			_, err := store.Ack(to.ID, to.Key, "../escape")
			return err
		}, "invalid_identity"},
		{"read messages with the wrong capability", func(_ *testing.T, store bus.Store, _, to agent) error {
			_, err := store.Messages(to.ID, "wrong")
			return err
		}, "unauthorized"},
		{"update to retired without retiring", func(_ *testing.T, store bus.Store, _, to agent) error {
			return store.Update(to.ID, model.StateRetired, false)
		}, "invalid_transition"},
		{"update to a live state while retiring", func(_ *testing.T, store bus.Store, _, to agent) error {
			return store.Update(to.ID, model.StateBusy, true)
		}, "invalid_transition"},
		{"register an unsupported agent kind", func(t *testing.T, store bus.Store, _, _ agent) error {
			t.Helper()
			_, _, err := store.Register("unknown", newUUID(t), "/workspace", nil)
			return err
		}, "invalid_input"},
		{"register with a native ID that could forge a record", func(_ *testing.T, store bus.Store, _, _ agent) error {
			_, _, err := store.Register("alpha", "id\nforged", "/workspace", nil)
			return err
		}, "invalid_identity"},
		{"use a store without an agent registry", func(t *testing.T, _ bus.Store, _, _ agent) error {
			t.Helper()
			_, _, err := bus.Store{Dir: filepath.Join(t.TempDir(), "state")}.Register("alpha", newUUID(t), "/workspace", nil)
			return err
		}, "internal"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newStore(t, nativeAdapter("alpha"), nativeAdapter("beta"))
			from, to := register(t, store, "alpha"), register(t, store, "beta")
			err := test.call(t, store, from, to)
			if err == nil {
				t.Fatalf("expected error code %q, got success", test.want)
			}
			if got := model.ErrorCodeOf(err); got != test.want {
				t.Fatalf("error code = %q (%v), want %q", got, err, test.want)
			}
		})
	}
}

func send(store bus.Store, request bus.SendRequest) error {
	_, err := (&bus.Service{Store: store}).Send(context.Background(), request)
	return err
}

func sendOK(t *testing.T, store bus.Store, from, to agent) model.Message {
	t.Helper()
	msg, err := (&bus.Service{Store: store}).Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "hi"})
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

// TestNewerStateFormatFencesWritesButNotReads pins the whole-state fence as the
// caller sees it: representative writes report unsupported_schema and upgrade-required,
// reads keep working, and nothing changes on disk.
func TestNewerStateFormatFencesWritesButNotReads(t *testing.T) {
	store := newStore(t, nativeAdapter("alpha"), nativeAdapter("beta"))
	from, to := register(t, store, "alpha"), register(t, store, "beta")
	marker := readJSONFile(t, filepath.Join(store.Dir, "state-formats.json"))
	formats := marker["formats"].(map[string]any)
	formats["messages"] = 99
	writeJSONFile(t, filepath.Join(store.Dir, "state-formats.json"), marker)
	before := snapshot(t, store.Dir)

	writes := map[string]func() error{
		"Send": func() error {
			return send(store, bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "hi"})
		},
		"Register": func() error { _, _, err := store.Register("alpha", newUUID(t), "/workspace", nil); return err },
		"Update":   func() error { return store.Update(to.ID, model.StateBusy, false) },
		"Prune":    func() error { _, err := store.Prune(false, nil); return err },
	}
	for name, write := range writes {
		err := write()
		if model.ErrorCodeOf(err) != "unsupported_schema" || !bus.IsUpgradeRequired(err) {
			t.Fatalf("%s: code %q upgrade-required %v (%v), want %q and true", name, model.ErrorCodeOf(err), bus.IsUpgradeRequired(err), err, "unsupported_schema")
		}
	}
	if err := store.CheckWriteCompatibility(); !bus.IsUpgradeRequired(err) {
		t.Fatalf("CheckWriteCompatibility = %v, want upgrade required", err)
	}
	if sessions, err := store.List(); err != nil || len(sessions) != 2 {
		t.Fatalf("List under the fence = %d sessions, %v", len(sessions), err)
	}
	if _, err := store.Messages(to.ID, to.Key); err != nil {
		t.Fatalf("Messages under the fence: %v", err)
	}
	if after := snapshot(t, store.Dir); !maps.Equal(before, after) {
		t.Fatal("a fenced write changed the state")
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "transactions")); !os.IsNotExist(err) {
		t.Fatal("a fenced operation created a directory", err)
	}
}

// TestUpdateOfAnUnknownSessionIsASilentNoOp pins a deliberate leniency: a
// lifecycle hook that arrives after its session was pruned must not fail.
func TestUpdateOfAnUnknownSessionIsASilentNoOp(t *testing.T) {
	store := newStore(t, nativeAdapter("alpha"))
	register(t, store, "alpha")
	before := snapshot(t, store.Dir)
	if err := store.Update("alpha:11111111-1111-4111-8111-111111111111", model.StateBusy, false); err != nil {
		t.Fatalf("Update of an unknown session: %v", err)
	}
	if after := snapshot(t, store.Dir); !maps.Equal(before, after) {
		t.Fatal("Update of an unknown session wrote state")
	}
}
