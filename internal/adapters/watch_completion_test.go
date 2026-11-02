package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

// Completion notices must not depend on which client produced the event or on
// native status strings: each adapter delivers the canonical event core expects.
func TestWatchCompletionIsDecidedFromCanonicalEventsForEveryAdapter(t *testing.T) {
	type hook struct{ event, status string }
	for _, tc := range []struct {
		name, kind string
		hook       hook
		want       string // substring of the notice body; empty means no notice
	}{
		{"claude stop", "claude", hook{"Stop", ""}, "finished a turn"},
		{"claude session end", "claude", hook{"SessionEnd", ""}, "ended the session"},
		{"codex stop", "codex", hook{"Stop", ""}, "finished a turn"},
		{"codex interrupt", "codex", hook{"Interrupt", ""}, ""},
		{"cursor completed stop", "cursor", hook{"stop", "completed"}, "finished a turn"},
		{"cursor stop without status", "cursor", hook{"stop", ""}, "finished a turn"},
		{"cursor aborted stop", "cursor", hook{"stop", "aborted"}, ""},
		{"cursor errored stop", "cursor", hook{"stop", "error"}, ""},
		{"cursor session end", "cursor", hook{"sessionEnd", "aborted"}, "ended the session"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := adapterTestStore(t)
			watcherKind := "codex"
			if tc.kind == "codex" {
				watcherKind = "claude"
			}
			watcher, key := registerAdapterSession(t, store, watcherKind)
			target, _ := registerAdapterSession(t, store, tc.kind)
			var bodies []string
			service := bus.Service{Store: store, Deliver: func(_ context.Context, msg bus.Message, _ bus.Session) bus.Receipt {
				bodies = append(bodies, msg.Body)
				return bus.NewReceipt(bus.StatusSubmitted)
			}}
			if result, err := service.Watch(watcher.ID, key, target.ID); err != nil || result.Status != "watching" {
				t.Fatal(result, err)
			}
			native := strings.TrimPrefix(target.ID, tc.kind+":")
			payload := map[string]any{"hook_event_name": tc.hook.event, "status": tc.hook.status}
			if tc.kind == "cursor" {
				payload["conversation_id"] = native
			} else {
				payload["session_id"] = native
			}
			input, _ := json.Marshal(payload)
			if err := service.Hook(tc.kind, bytes.NewReader(input), &bytes.Buffer{}); err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(bodies) != 0 {
					t.Fatalf("unexpected notice: %q", bodies)
				}
				// The watch stays armed for the turn that does finish.
				if result, err := service.Watch(watcher.ID, key, target.ID); err != nil || result.Status != "watching" {
					t.Fatal(result, err)
				}
				return
			}
			if len(bodies) != 1 || !strings.Contains(bodies[0], tc.want) {
				t.Fatalf("notices: %q, want one containing %q", bodies, tc.want)
			}
		})
	}
}
