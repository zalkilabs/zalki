package bus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// splitEvents is an adapter whose client has two native events, "before" and
// "after", that both mean PostToolUse. It records what each call saw.
type splitEvents struct {
	stubCodexAdapter
	seen *[]string
	// forge makes NormalizeHook overwrite NativeEvent, to show later calls do not see it.
	forge bool
}

func (a splitEvents) Descriptor() agent.AdapterDescriptor {
	d := a.stubCodexAdapter.Descriptor()
	d.Hooks = map[string]string{"begin": "SessionStart", "before": "PostToolUse", "after": "PostToolUse", "end": "Stop"}
	return d
}

func (a splitEvents) record(call string, event agent.HookInput) {
	*a.seen = append(*a.seen, fmt.Sprintf("%s %s/%s", call, event.Event, event.NativeEvent))
}

func (a splitEvents) NormalizeHook(event agent.HookInput) (agent.HookInput, error) {
	a.record("normalize", event)
	if a.forge {
		event.NativeEvent = "forged"
	}
	return event, nil
}

func (a splitEvents) RegisterHook(event agent.HookInput) (agent.HookRegistration, error) {
	a.record("register", event)
	return agent.HookRegistration{}, nil
}

func (a splitEvents) HandleHook(_ agent.HookHost, event agent.HookInput, _ string, _ io.Writer) (bool, error) {
	a.record("handle", event)
	return false, nil
}

func hookCalls(t *testing.T, adapter splitEvents, payloads ...map[string]any) {
	t.Helper()
	registry, err := agent.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: Store{dir: filepath.Join(t.TempDir(), "state"), registry: registry}}
	for _, payload := range payloads {
		input, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Hook(t.Context(), "codex", bytes.NewReader(input), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
}

func hookPayloadFor(id, native string, extra map[string]any) map[string]any {
	payload := map[string]any{"hook_event_name": native, "session_id": id, "cwd": "/workspace"}
	for k, v := range extra {
		payload[k] = v
	}
	return payload
}

// Two native events that map to one common event are told apart by the adapter,
// in every call that receives the event.
func TestAdapterSeesTheNativeEventNameBehindOneCommonEvent(t *testing.T) {
	var seen []string
	id := newID()
	hookCalls(t, splitEvents{seen: &seen},
		hookPayloadFor(id, "begin", nil), hookPayloadFor(id, "before", nil), hookPayloadFor(id, "after", nil), hookPayloadFor(id, "end", nil))
	want := []string{
		"normalize SessionStart/begin", "handle SessionStart/begin", "register SessionStart/begin",
		"normalize PostToolUse/before", "handle PostToolUse/before",
		"normalize PostToolUse/after", "handle PostToolUse/after",
		"normalize Stop/end", "handle Stop/end",
	}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("adapter calls:\n%q\nwant:\n%q", seen, want)
	}
}

// The wire cannot set the field: only core does.
func TestNativeEventCannotBeSetFromTheInput(t *testing.T) {
	var seen []string
	id := newID()
	hookCalls(t, splitEvents{seen: &seen}, hookPayloadFor(id, "begin", nil),
		hookPayloadFor(id, "before", map[string]any{"NativeEvent": "forged", "native_event": "forged"}))
	for _, call := range seen[len(seen)-2:] {
		if call != "normalize PostToolUse/before" && call != "handle PostToolUse/before" {
			t.Fatalf("a field of the input reached the adapter as the native event: %q", seen)
		}
	}
}

// An adapter that changes the field in NormalizeHook does not change what core
// passes on to the later calls.
func TestNativeEventIsCoreOwnedAfterNormalize(t *testing.T) {
	var seen []string
	id := newID()
	hookCalls(t, splitEvents{seen: &seen, forge: true}, hookPayloadFor(id, "begin", nil), hookPayloadFor(id, "after", nil))
	want := []string{"normalize SessionStart/begin", "handle SessionStart/begin", "register SessionStart/begin", "normalize PostToolUse/after", "handle PostToolUse/after"}
	if !reflect.DeepEqual(seen, want) {
		t.Fatalf("adapter calls:\n%q\nwant:\n%q", seen, want)
	}
}
