package bus_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// These expectations are literal wire data, independent of reflected Go fields.
// Compare compact bytes so field order, tags, omission and null remain visible.
func requireModelJSON(t *testing.T, value any, want string) {
	t.Helper()
	got, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(want)); err != nil {
		t.Fatalf("invalid literal expectation: %v", err)
	}
	if string(got) != compact.String() {
		t.Fatalf("JSON changed\ngot  %s\nwant %s", got, compact.String())
	}
}

const zeroModelReceiveJSON = `{"idle_wake":false,"delivery":"","inbox":"","busy_delivery":"","lifecycle_hooks":false,"hook_delivery":false,"requires_listener":false,"requires_endpoint":false,"experimental":false}`
const fullModelReceiveJSON = `{"idle_wake":true,"delivery":"fixture","inbox":"journal","busy_delivery":"next_hook","lifecycle_hooks":true,"hook_delivery":true,"requires_listener":true,"requires_endpoint":true,"experimental":true}`
const zeroModelSessionJSON = `{"schema":0,"id":"","kind":"","cwd":"","generation":"","state":"","updated_at":"","retired":false,"receive":` + zeroModelReceiveJSON + `}`

func fullModelReceive() bus.ReceiveCapabilities {
	return bus.ReceiveCapabilities{IdleWake: true, Delivery: "fixture", Inbox: bus.InboxJournal, BusyDelivery: bus.DeliveryNextHook,
		LifecycleHooks: true, HookDelivery: true, RequiresListener: true, RequiresEndpoint: true, Experimental: true}
}

func fullModelReceipt() bus.Receipt {
	r := bus.Receipt{At: "at", Status: bus.StatusSubmitted, Code: bus.CodeUnavailable, Error: "public error", Output: "output"}
	return r.WithCause(errors.New("live cause must not serialize"))
}

func fullModelSession() bus.Session {
	return bus.Session{
		Schema: 2, NativeID: "native", AdapterState: json.RawMessage(`{"schema":1,"flag":true}`), ID: "alpha:id", Kind: "alpha",
		Name: "Name", NameSource: "fixture", Task: "Task", NativeConfigDir: "/config", CWD: "/workspace", Generation: "generation",
		State: bus.StateBusy, UpdatedAt: "updated", Retired: true, RetiredReason: "done", ApprovalHeld: 2,
		Owner: &model.ProcessRef{PID: 7, Start: "start", Role: "client"}, Liveness: bus.LivenessAlive, HookBuild: "build",
		Endpoint: &bus.Endpoint{Path: "/socket", Device: 3, Inode: 5, ChangedNS: 7},
		Listener: &bus.ListenerLease{ID: "lease", Generation: "generation", Endpoint: bus.Endpoint{Path: "/listener", Device: 11, Inode: 13, ChangedNS: 17}},
		KeyHash:  "hash", Receive: fullModelReceive(),
	}
}

func TestModelSessionJSONSurface(t *testing.T) {
	for _, test := range []struct {
		name  string
		value bus.Session
		want  string
	}{
		{"zero", bus.Session{}, zeroModelSessionJSON},
		{"empty raw state", bus.Session{AdapterState: json.RawMessage{}}, zeroModelSessionJSON},
		{"explicit raw null", bus.Session{AdapterState: json.RawMessage(`null`)},
			`{"schema":0,"adapter_state":null,"id":"","kind":"","cwd":"","generation":"","state":"","updated_at":"","retired":false,"receive":` + zeroModelReceiveJSON + `}`},
		{"non-nil zero pointers", bus.Session{Owner: &model.ProcessRef{}, Endpoint: &bus.Endpoint{}, Listener: &bus.ListenerLease{}},
			`{"schema":0,"id":"","kind":"","cwd":"","generation":"","state":"","updated_at":"","retired":false,"owner":{"pid":0,"start":"","role":""},"endpoint":{"path":"","device":0,"inode":0,"changed_ns":0},"listener":{"id":"","generation":"","endpoint":{"path":"","device":0,"inode":0,"changed_ns":0}},"receive":` + zeroModelReceiveJSON + `}`},
		{"all fields", fullModelSession(),
			`{"schema":2,"native_id":"native","adapter_state":{"schema":1,"flag":true},"id":"alpha:id","kind":"alpha","name":"Name","name_source":"fixture","task":"Task","native_config_dir":"/config","cwd":"/workspace","generation":"generation","state":"busy","updated_at":"updated","retired":true,"retired_reason":"done","approval_held":2,"owner":{"pid":7,"start":"start","role":"client"},"liveness":"alive","hook_build":"build","endpoint":{"path":"/socket","device":3,"inode":5,"changed_ns":7},"listener":{"id":"lease","generation":"generation","endpoint":{"path":"/listener","device":11,"inode":13,"changed_ns":17}},"key_hash":"hash","receive":` + fullModelReceiveJSON + `}`},
	} {
		t.Run(test.name, func(t *testing.T) { requireModelJSON(t, test.value, test.want) })
	}
}

func TestModelMessageJSONSurface(t *testing.T) {
	receipt := fullModelReceipt()
	for _, test := range []struct {
		name  string
		value bus.Message
		want  string
	}{
		{"zero", bus.Message{}, `{"schema":0,"id":"","from":"","to":"","body":"","target_generation":"","created_at":"","status":""}`},
		{"non-nil zero receipt", bus.Message{Receipt: &bus.Receipt{}}, `{"schema":0,"id":"","from":"","to":"","body":"","target_generation":"","created_at":"","status":"","receipt":{"at":"","status":""}}`},
		{"all fields", bus.Message{
			BusyDelivery: bus.DeliveryNextTurn, BodyPreview: "not serialized", Schema: 1, ID: "message", From: "alpha:from", Kind: "notice",
			FromName: "Sender", FromLabel: "Alpha", To: "beta:to", RequestedTo: "Alias", Body: "body", BodyOmitted: true, BodyDeferred: true,
			ReplyTo: "parent", TargetGeneration: "generation", CreatedAt: "created", Status: bus.StatusSubmitted, Held: true, Receipt: &receipt, AcknowledgedAt: "acknowledged",
		}, `{"schema":1,"id":"message","from":"alpha:from","kind":"notice","from_name":"Sender","from_label":"Alpha","to":"beta:to","requested_to":"Alias","body":"body","body_omitted":true,"body_deferred":true,"reply_to":"parent","target_generation":"generation","created_at":"created","status":"submitted","held":true,"receipt":{"at":"at","status":"submitted","code":"unavailable","error":"public error","output":"output"},"acknowledged_at":"acknowledged"}`},
	} {
		t.Run(test.name, func(t *testing.T) { requireModelJSON(t, test.value, test.want) })
	}
}

func TestModelReceiptJSONSurface(t *testing.T) {
	for _, test := range []struct {
		name  string
		value bus.Receipt
		want  string
	}{
		{"zero", bus.Receipt{}, `{"at":"","status":""}`},
		{"all fields and live cause", fullModelReceipt(), `{"at":"at","status":"submitted","code":"unavailable","error":"public error","output":"output"}`},
	} {
		t.Run(test.name, func(t *testing.T) { requireModelJSON(t, test.value, test.want) })
	}
}

func TestModelDiagnosticJSONSurface(t *testing.T) {
	for _, test := range []struct {
		name  string
		value model.SessionDiagnostic
		want  string
	}{
		{"zero", model.SessionDiagnostic{}, `{"id":"","kind":"","state":"","retired":false,"liveness":"","updated_at":"","transport_health":"","hook_build_status":"","receive":` + zeroModelReceiveJSON + `}`},
		{"non-nil zero owner", model.SessionDiagnostic{Owner: &model.ProcessRef{}}, `{"id":"","kind":"","state":"","retired":false,"liveness":"","owner":{"pid":0,"start":"","role":""},"updated_at":"","transport_health":"","hook_build_status":"","receive":` + zeroModelReceiveJSON + `}`},
		{"all fields", model.SessionDiagnostic{ID: "alpha:id", Kind: "alpha", Name: "Name", State: bus.StateRetired, Retired: true,
			RetiredReason: "done", Liveness: bus.LivenessExited, Owner: &model.ProcessRef{PID: 7, Start: "start", Role: "client"}, UpdatedAt: "updated",
			TransportHealth: "transport", ListenerHealth: "listener", HookBuild: "build", HookBuildStatus: "current", RecoveryHint: "hint", Receive: fullModelReceive()},
			`{"id":"alpha:id","kind":"alpha","name":"Name","state":"retired","retired":true,"retired_reason":"done","liveness":"exited","owner":{"pid":7,"start":"start","role":"client"},"updated_at":"updated","transport_health":"transport","listener_health":"listener","hook_build":"build","hook_build_status":"current","recovery_hint":"hint","receive":` + fullModelReceiveJSON + `}`},
	} {
		t.Run(test.name, func(t *testing.T) { requireModelJSON(t, test.value, test.want) })
	}
}
