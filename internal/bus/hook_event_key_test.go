package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// camelAdapter stands in for a client that sends hookEventName and conversationId
// instead of hook_event_name. It declares its own key and one native field.
type camelAdapter struct{ stubStartup }

func (camelAdapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{
		Kind: "camel", Label: "Camel", HookEventKey: "hookEventName", OwnerRole: "client",
		Hooks:  map[string]string{"onStart": "SessionStart", "onPromptDone": "Stop"},
		Native: []agent.NativeField{{Key: "conversationId", Kind: agent.NativeString, Capture: agent.CaptureID}},
	}
}

func (camelAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "test", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, LifecycleHooks: true}
}

func (camelAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}

func (camelAdapter) NormalizeHook(event agent.HookInput) (agent.HookInput, error) {
	var native struct {
		ConversationID string `json:"conversationId"`
	}
	_ = json.Unmarshal(event.Native, &native)
	if native.ConversationID != "" {
		event.SessionID = native.ConversationID
	}
	return event, nil
}

func camelService(t *testing.T) Service {
	t.Helper()
	registry, err := agent.NewRegistry(camelAdapter{}, stubClaudeAdapter{}, stubCodexAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	return Service{Store: Store{dir: filepath.Join(t.TempDir(), "state"), registry: registry}}
}

func decodeCamel(t *testing.T, adapter agent.Adapter, payload string) (agent.HookInput, hookTrace, error) {
	t.Helper()
	var trace hookTrace
	service := camelService(t)
	event, _, err := service.decodeHook(context.Background(), adapter.Descriptor().Kind, strings.NewReader(payload), adapter, adapter.(agent.HookAdapter), &trace)
	return event, trace, err
}

func TestHookReadsTheEventUnderTheDescriptorKey(t *testing.T) {
	for _, test := range []struct {
		name, payload, event string
	}{
		{"the descriptor key", `{"hookEventName":"onPromptDone","conversationId":"C"}`, "Stop"},
		{"the key ignores case like the common fields", `{"HOOKEVENTNAME":"onStart","conversationId":"C"}`, "SessionStart"},
		{"the last duplicate wins like for the common fields", `{"hookEventName":"onStart","hookEventName":"onPromptDone","conversationId":"C"}`, "Stop"},
		{"a null value is absent", `{"hookEventName":null}`, ""},
		// The typed decode reads hook_event_name by itself; for this adapter it must not count.
		{"hook_event_name is not read", `{"hook_event_name":"onStart","hookEventName":"onPromptDone","conversationId":"C"}`, "Stop"},
	} {
		event, trace, err := decodeCamel(t, camelAdapter{}, test.payload)
		if test.event == "" {
			if err == nil || err.Error() != "unsupported agent lifecycle event" {
				t.Fatal(test.name, err)
			}
			continue
		}
		if err != nil || event.Event != test.event || trace.lifecycle != test.event {
			t.Fatal(test.name, event.Event, trace.lifecycle, err)
		}
	}
}

func TestHookKeepsTheEventKeyOutOfNativeFields(t *testing.T) {
	event, _, err := decodeCamel(t, camelAdapter{}, `{"hookEventName":"onStart","conversationId":"C"}`)
	if err != nil || string(event.Native) != `{"conversationId":"C"}` || event.SessionID != "C" {
		t.Fatal(string(event.Native), event.SessionID, err)
	}
}

func TestHookRefusesWhatTheDeclaredKeyDoesNotName(t *testing.T) {
	for _, test := range []struct {
		name, payload, want string
	}{
		{"only hook_event_name", `{"hook_event_name":"onStart"}`, "unsupported agent lifecycle event"},
		{"a number", `{"hookEventName":5}`, "hook field hookEventName has the wrong type"},
		{"no event at all", `{"conversationId":"C"}`, "unsupported agent lifecycle event"},
		{"an empty event", `{"hookEventName":""}`, "unsupported agent lifecycle event"},
		{"an unknown event", `{"hookEventName":"nope"}`, "unsupported agent lifecycle event"},
	} {
		_, _, err := decodeCamel(t, camelAdapter{}, test.payload)
		if err == nil || err.Error() != test.want {
			t.Fatal(test.name, err)
		}
	}
}

// Adapters that declare hook_event_name read that key, the way they always did,
// and not the camelCase one.
func TestHookEventKeyOfTheStandardAdapters(t *testing.T) {
	for _, adapter := range []agent.Adapter{stubClaudeAdapter{}, stubCodexAdapter{}} {
		event, _, err := decodeCamel(t, adapter, `{"hook_event_name":"Stop","hookEventName":"SessionStart","session_id":"S"}`)
		if err != nil || event.Event != "Stop" {
			t.Fatal(adapter.Descriptor().Kind, event.Event, err)
		}
		_, _, err = decodeCamel(t, adapter, `{"hookEventName":"Stop","session_id":"S"}`)
		if err == nil || err.Error() != "unsupported agent lifecycle event" {
			t.Fatal(adapter.Descriptor().Kind, err)
		}
	}
}

// An adapter with its own key and no native fields still gets its key read.
type plainCamelAdapter struct{ camelAdapter }

func (plainCamelAdapter) Descriptor() agent.AdapterDescriptor {
	d := camelAdapter{}.Descriptor()
	d.Native = nil
	return d
}

func TestHookEventKeyWithoutNativeFields(t *testing.T) {
	event, _, err := decodeCamel(t, plainCamelAdapter{}, `{"hookEventName":"onPromptDone","conversationId":"C","session_id":"S"}`)
	if err != nil || event.Event != "Stop" || len(event.Native) != 0 {
		t.Fatal(event.Event, string(event.Native), err)
	}
}

// Core has no default: an adapter with hooks and no key is refused, by name.
type noKeyAdapter struct{ camelAdapter }

func (noKeyAdapter) Descriptor() agent.AdapterDescriptor {
	d := camelAdapter{}.Descriptor()
	d.HookEventKey = ""
	return d
}

func TestHookRefusesAnAdapterWithoutAnEventKey(t *testing.T) {
	_, _, err := decodeCamel(t, noKeyAdapter{}, `{"hook_event_name":"onStart","hookEventName":"onStart"}`)
	want := `adapter "camel" has hooks but declares no HookEventKey; set it in its descriptor to the JSON key its client sends the event name in (for example "hook_event_name")`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatal(err)
	}
}

// The fast path relies on HookInput decoding the same key by itself.
func TestStructEventKeyIsTheTagOfHookInputEvent(t *testing.T) {
	field, ok := reflect.TypeOf(agent.HookInput{}).FieldByName("Event")
	if !ok || field.Tag.Get("json") != structEventKey {
		t.Fatal("HookInput.Event tag is", field.Tag.Get("json"), "want", structEventKey)
	}
}

// The streaming selector (Codex) reads a camelCase key from its kept metadata.
type streamingKeyAdapter struct{ stubCodexAdapter }

func (streamingKeyAdapter) Descriptor() agent.AdapterDescriptor {
	d := stubCodexAdapter{}.Descriptor()
	d.HookEventKey = "hookEventName"
	return d
}

func TestHookEventKeyWorksWithTheStreamingSelector(t *testing.T) {
	event, _, err := decodeCamel(t, streamingKeyAdapter{}, `{"hook_event_name":"SessionStart","hookEventName":"Stop","tool_input":{"hookEventName":"nope"},"session_id":"S"}`)
	if err != nil || event.Event != "Stop" {
		t.Fatal(event.Event, err)
	}
	// The size rule of the selector follows the declared key as well.
	_, _, err = decodeCamel(t, streamingKeyAdapter{}, `{"hookEventName":"Stop","session_id":"S","prompt":"`+strings.Repeat("x", maxHookPayload)+`"}`)
	if err == nil || !strings.Contains(err.Error(), "8 MiB") {
		t.Fatal(err)
	}
}

func TestHookFailureLogsTheEventFromTheDescriptorKey(t *testing.T) {
	_, trace, err := decodeCamel(t, camelAdapter{}, `{"hookEventName":"onPromptDone","conversationId":`)
	if err == nil || trace.lifecycle != "Stop" {
		t.Fatal(trace.lifecycle, err)
	}
	// A type error in another field still leaves the event name for the log.
	_, trace, err = decodeCamel(t, camelAdapter{}, `{"hookEventName":"onPromptDone","session_id":5}`)
	if err == nil || trace.lifecycle != "Stop" {
		t.Fatal(trace.lifecycle, err)
	}
	// The streaming selector fails before the typed decode; the log reads the key from its header.
	_, trace, err = decodeCamel(t, streamingKeyAdapter{}, `{"hookEventName":"Stop","session_id":`)
	if err == nil || trace.lifecycle != "Stop" {
		t.Fatal(trace.lifecycle, err)
	}
	_, trace, err = decodeCamel(t, stubClaudeAdapter{}, `{"hookEventName":"Stop","session_id":`)
	if err == nil || trace.lifecycle != "unknown" {
		t.Fatal(trace.lifecycle, err)
	}
}

func TestProbeHookEventUsesTheGivenKey(t *testing.T) {
	const header = `{"hookEventName":"onStart","hook_event_name":"other"`
	if got := probeHookEvent([]byte(header), "hookEventName"); got != "onStart" {
		t.Fatal(got)
	}
	if got := probeHookEvent([]byte(header), structEventKey); got != "other" {
		t.Fatal(got)
	}
}

func TestSelectEventName(t *testing.T) {
	for _, test := range []struct {
		name, payload, want string
		wantErr             bool
	}{
		{"present", `{"a":1,"hookEventName":"x"}`, "x", false},
		{"not at the top level", `{"nested":{"hookEventName":"x"}}`, "", false},
		{"null is absent", `{"hookEventName":null}`, "", false},
		{"not an object", `[1]`, "", false},
		{"trailing input is not read", `{"hookEventName":"x"} {"hookEventName":"y"}`, "x", false},
		{"the last duplicate wins", `{"hookEventName":"x","hookeventname":"y"}`, "y", false},
		{"wrong type", `{"hookEventName":["x"]}`, "", true},
	} {
		got, err := selectEventName([]byte(test.payload), "hookEventName")
		if (err != nil) != test.wantErr || got != test.want {
			t.Fatal(test.name, got, err)
		}
	}
}

func TestContractCaptureWritesTheDescriptorEventKey(t *testing.T) {
	root := filepath.Join(t.TempDir(), "contracts")
	t.Setenv("ZALKI_CAPTURE_CONTRACTS", root)
	t.Setenv("PATH", t.TempDir())
	event := agent.HookInput{Event: "onStart", SessionID: "private", CWD: "/private/project"}
	if err := captureHookContract(t.Context(), camelAdapter{}, event); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "camel-unknown.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Hook map[string]any `json:"hook"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.Hook["hookEventName"] != "onStart" || fixture.Hook["hook_event_name"] != nil {
		t.Fatal("fixture hook:", fixture.Hook)
	}
}

// A client with its own key can run the whole lifecycle: register, then stop.
func TestHookRunsTheLifecycleOfAClientWithItsOwnEventKey(t *testing.T) {
	service := camelService(t)
	var output bytes.Buffer
	start := `{"hookEventName":"onStart","conversationId":"6f1d2c3a-4b5e-4f60-8a7b-9c0d1e2f3a4b","cwd":"/workspace"}`
	if err := service.Hook(context.Background(), "camel", strings.NewReader(start), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Zalki actor: camel:6f1d2c3a-4b5e-4f60-8a7b-9c0d1e2f3a4b") {
		t.Fatal("startup output has no actor:", output.String())
	}
	stop := `{"hookEventName":"onPromptDone","conversationId":"6f1d2c3a-4b5e-4f60-8a7b-9c0d1e2f3a4b"}`
	if err := service.Hook(context.Background(), "camel", strings.NewReader(stop), &output); err != nil {
		t.Fatal(err)
	}
	session, err := service.Store.session("camel:6f1d2c3a-4b5e-4f60-8a7b-9c0d1e2f3a4b")
	if err != nil || session.State != model.StateReady {
		t.Fatal(session.State, err)
	}
}
