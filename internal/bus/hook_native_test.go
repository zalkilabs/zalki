package bus

import (
	"context"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// streamingNativeAdapter declares native fields although it uses the streaming
// selector, which is not supported: core keeps nothing for it.
type streamingNativeAdapter struct{ stubCodexAdapter }

func (streamingNativeAdapter) Descriptor() agent.AdapterDescriptor {
	d := stubCodexAdapter{}.Descriptor()
	d.Native = stubNativeFields
	return d
}

func TestSelectNativeKeepsOnlyDeclaredKeys(t *testing.T) {
	fields := stubNativeFields
	for _, test := range []struct {
		name, payload, want string
		wantErr             bool
	}{
		{"declared keys only", `{"prompt":"private","tool_input":{"a":1},"status":"completed","conversation_id":"C","loop_count":2}`, `{"conversation_id":"C","loop_count":2,"status":"completed"}`, false},
		{"none present", `{"prompt":"private"}`, ``, false},
		{"null is absent", `{"status":null,"conversation_id":"C"}`, `{"conversation_id":"C"}`, false},
		{"key case is ignored like the common fields", `{"Conversation_ID":"C"}`, `{"conversation_id":"C"}`, false},
		{"last duplicate wins", `{"status":"a","status":"b"}`, `{"status":"b"}`, false},
		{"empty list is kept", `{"workspace_roots":[ ]}`, `{"workspace_roots":[]}`, false},
		{"not an object", `[1]`, ``, false},
		{"null payload", `null`, ``, false},
		{"string expected", `{"conversation_id":5}`, ``, true},
		{"number expected", `{"loop_count":"x"}`, ``, true},
		{"fraction is not an int", `{"loop_count":1.5}`, ``, true},
		{"list of strings expected", `{"workspace_roots":["a",1]}`, ``, true},
		{"trailing input is not read", `{"status":"a"} {"status":`, `{"status":"a"}`, false},
	} {
		got, err := selectNative([]byte(test.payload), fields)
		if (err != nil) != test.wantErr || string(got) != test.want {
			t.Fatal(test.name, string(got), err)
		}
	}
}

func TestDecodeHookKeepsNativeFieldsOnlyForDeclaringAdapters(t *testing.T) {
	const payload = `{"session_id":"S","conversation_id":"C","status":"aborted","hook_event_name":"%s","prompt":"private"}`
	for _, test := range []struct {
		name    string
		adapter agent.Adapter
		native  string
	}{
		{"declaring adapter", stubCursorAdapter{}, `{"conversation_id":"C","status":"aborted"}`},
		{"adapter without native fields", stubClaudeAdapter{}, ``},
		{"adapter with the streaming selector", stubCodexAdapter{}, ``},
		{"native fields are not kept with the streaming selector", streamingNativeAdapter{}, ``},
	} {
		event := "stop"
		if test.adapter.Descriptor().Kind != "cursor" {
			event = "Stop"
		}
		service := newTestService(t)
		var trace hookTrace
		got, _, err := service.decodeHook(context.Background(), test.adapter.Descriptor().Kind, strings.NewReader(strings.Replace(payload, "%s", event, 1)), test.adapter, test.adapter.(agent.HookAdapter), &trace)
		if err != nil || string(got.Native) != test.native {
			t.Fatal(test.name, string(got.Native), err)
		}
	}
}

// A native field of the wrong type fails the hook at the decode stage, as a
// wrong common field does.
func TestDecodeHookRejectsAWrongNativeType(t *testing.T) {
	service := newTestService(t)
	var trace hookTrace
	adapter := stubCursorAdapter{}
	_, _, err := service.decodeHook(context.Background(), "cursor", strings.NewReader(`{"conversation_id":"C","loop_count":"x","hook_event_name":"stop"}`), adapter, adapter, &trace)
	if err == nil || trace.stage != "decode" || !strings.Contains(err.Error(), "loop_count") {
		t.Fatal("a wrong native type was accepted or failed at another stage", trace.stage, err)
	}
	_, _, err = service.decodeHook(context.Background(), "cursor", strings.NewReader(`{"session_id":5,"conversation_id":"C","hook_event_name":"stop"}`), adapter, adapter, &trace)
	if err == nil {
		t.Fatal("a wrong common type was accepted")
	}
}
