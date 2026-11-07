package cursor

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// Core decides completion from canonical events, so the adapter must turn each
// native Stop status into a canonical event.
func TestNormalizeHookMapsNativeStopStatusToCanonicalEvents(t *testing.T) {
	for _, tc := range []struct {
		name, event, status, want string
	}{
		{"completed", "Stop", "completed", "Stop"},
		{"no status", "Stop", "", "Stop"},
		{"aborted", "Stop", "aborted", "Interrupt"},
		{"error", "Stop", "error", "Interrupt"},
		{"unknown status", "Stop", "something-new", "Interrupt"},
		{"other event ignores status", "PostToolUse", "aborted", "PostToolUse"},
		{"session end ignores status", "SessionEnd", "aborted", "SessionEnd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Adapter{}.NormalizeHook(agent.HookInput{Event: tc.event, Status: tc.status})
			if err != nil || got.Event != tc.want {
				t.Fatalf("event %q status %q: got %q, %v; want %q", tc.event, tc.status, got.Event, err, tc.want)
			}
		})
	}
}
