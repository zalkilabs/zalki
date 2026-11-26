package codex

import "testing"

// Codex sends the event name as hook_event_name. Core reads only the key the
// descriptor declares, so this must stay declared.
func TestDescriptorDeclaresTheHookEventKey(t *testing.T) {
	if got := New(false, "fixture.json").Descriptor().HookEventKey; got != "hook_event_name" {
		t.Fatalf("HookEventKey = %q, want hook_event_name", got)
	}
}
