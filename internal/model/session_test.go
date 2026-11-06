// These model tests pin Session's persisted shape using literal JSON, while bus
// characterization tests independently cover the compatibility alias and codec.
package model

import (
	"encoding/json"
	"testing"
)

const sessionReceiveZero = `{"idle_wake":false,"delivery":"","inbox":"","busy_delivery":"","lifecycle_hooks":false,"hook_delivery":false,"requires_listener":false,"requires_endpoint":false,"experimental":false}`
const sessionZero = `{"schema":0,"id":"","kind":"","cwd":"","generation":"","state":"","updated_at":"","retired":false,"receive":` + sessionReceiveZero + `}`

func TestSessionZeroAndOmissions(t *testing.T) {
	for _, test := range []struct {
		name  string
		value Session
		want  string
	}{
		{"zero", Session{}, sessionZero},
		{"empty raw", Session{AdapterState: json.RawMessage{}}, sessionZero},
		{"raw null", Session{AdapterState: json.RawMessage(`null`)}, `{"schema":0,"adapter_state":null,"id":"","kind":"","cwd":"","generation":"","state":"","updated_at":"","retired":false,"receive":` + sessionReceiveZero + `}`},
		{"zero pointers", Session{Owner: &ProcessRef{}, Endpoint: &Endpoint{}, Listener: &ListenerLease{}}, `{"schema":0,"id":"","kind":"","cwd":"","generation":"","state":"","updated_at":"","retired":false,"owner":{"pid":0,"start":"","role":""},"endpoint":{"path":"","device":0,"inode":0,"changed_ns":0},"listener":{"id":"","generation":"","endpoint":{"path":"","device":0,"inode":0,"changed_ns":0}},"receive":` + sessionReceiveZero + `}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := json.Marshal(test.value)
			if err != nil || string(got) != test.want {
				t.Fatalf("session JSON=%s, error=%v, want %s", got, err, test.want)
			}
		})
	}
}

func TestSessionFullJSON(t *testing.T) {
	value := Session{
		Schema: 2, NativeID: "native", AdapterState: json.RawMessage(`{"schema":1}`), ID: "alpha:id", Kind: "alpha",
		Name: "Name", NameSource: "fixture", Task: "Task", NativeConfigDir: "/config", CWD: "/work", Generation: "gen",
		State: "busy", UpdatedAt: "now", Retired: true, RetiredReason: "done", ApprovalHeld: 2,
		Owner: &ProcessRef{PID: 7, Start: "start", Role: "client"}, Liveness: "alive", HookBuild: "build",
		Endpoint: &Endpoint{Path: "/socket", Device: 3, Inode: 5, ChangedNS: 7},
		Listener: &ListenerLease{ID: "lease", Generation: "gen", Endpoint: Endpoint{Path: "/listener", Device: 11, Inode: 13, ChangedNS: 17}},
		KeyHash:  "private hash", Receive: ReceiveCapabilities{IdleWake: true, Delivery: "fixture", Inbox: "journal", BusyDelivery: "next_hook", LifecycleHooks: true, HookDelivery: true, RequiresListener: true, RequiresEndpoint: true, Experimental: true},
	}
	const want = `{"schema":2,"native_id":"native","adapter_state":{"schema":1},"id":"alpha:id","kind":"alpha","name":"Name","name_source":"fixture","task":"Task","native_config_dir":"/config","cwd":"/work","generation":"gen","state":"busy","updated_at":"now","retired":true,"retired_reason":"done","approval_held":2,"owner":{"pid":7,"start":"start","role":"client"},"liveness":"alive","hook_build":"build","endpoint":{"path":"/socket","device":3,"inode":5,"changed_ns":7},"listener":{"id":"lease","generation":"gen","endpoint":{"path":"/listener","device":11,"inode":13,"changed_ns":17}},"key_hash":"private hash","receive":{"idle_wake":true,"delivery":"fixture","inbox":"journal","busy_delivery":"next_hook","lifecycle_hooks":true,"hook_delivery":true,"requires_listener":true,"requires_endpoint":true,"experimental":true}}`
	got, err := json.Marshal(value)
	if err != nil || string(got) != want {
		t.Fatalf("full session JSON=%s, error=%v, want %s", got, err, want)
	}
	var decoded Session
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(decoded)
	if err != nil || string(again) != want {
		t.Fatalf("round trip=%s, error=%v, want %s", again, err, want)
	}
}

func TestSessionCopyKeepsPointersAndReceiveValue(t *testing.T) {
	original := Session{Owner: &ProcessRef{PID: 1}, Endpoint: &Endpoint{Path: "/socket"}, Listener: &ListenerLease{ID: "lease"}, AdapterState: json.RawMessage(`{"n":1}`), Receive: ReceiveCapabilities{IdleWake: true}}
	copied := original
	if copied.Owner != original.Owner || copied.Endpoint != original.Endpoint || copied.Listener != original.Listener {
		t.Fatal("copy changed pointer identity")
	}
	copied.Owner.PID, copied.Endpoint.Path, copied.Listener.ID = 2, "/changed", "changed"
	copied.AdapterState[5] = '2'
	copied.Receive.IdleWake = false
	if original.Owner.PID != 2 || original.Endpoint.Path != "/changed" || original.Listener.ID != "changed" || string(original.AdapterState) != `{"n":2}` {
		t.Fatal("Session no longer has ordinary shallow-copy semantics")
	}
	if !original.Receive.IdleWake {
		t.Fatal("copy unexpectedly shares Receive capabilities")
	}
}
