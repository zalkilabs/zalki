package agent

import (
	"reflect"
	"testing"
)

// StandardEvents is the vocabulary of native event names that Claude and Codex
// accept (their native names equal the common ones). Every adapter that builds
// its Hooks map from it accepts exactly these names, so the list is pinned.
func TestStandardEventsAreExactlyTheCurrentSix(t *testing.T) {
	want := map[string]string{
		"SessionStart": "SessionStart", "SessionEnd": "SessionEnd", "PermissionRequest": "PermissionRequest",
		"PostToolUse": "PostToolUse", "UserPromptSubmit": "UserPromptSubmit", "Stop": "Stop",
	}
	if got := StandardEvents(false); !reflect.DeepEqual(got, want) {
		t.Fatalf("StandardEvents(false) = %v, want %v", got, want)
	}
	want["Interrupt"] = "Interrupt"
	if got := StandardEvents(true); !reflect.DeepEqual(got, want) {
		t.Fatalf("StandardEvents(true) = %v, want %v", got, want)
	}
}

// The result is a fresh map: a caller that changes it must not change the next call.
func TestStandardEventsReturnsAFreshMap(t *testing.T) {
	first := StandardEvents(true)
	first["UnknownEvent"] = "UnknownEvent"
	delete(first, "Stop")
	if got := StandardEvents(true); got["UnknownEvent"] != "" || got["Stop"] != "Stop" {
		t.Fatalf("a caller changed the vocabulary: %v", got)
	}
}
