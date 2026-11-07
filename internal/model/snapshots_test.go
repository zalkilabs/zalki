// These tests pin the privacy boundary and literal diagnostic wire shape.
package model

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSessionViewCredentialBoundary(t *testing.T) {
	want := []struct {
		name   string
		typeOf reflect.Type
	}{
		{"Actor", reflect.TypeOf("")}, {"Kind", reflect.TypeOf("")}, {"Generation", reflect.TypeOf("")},
		{"State", reflect.TypeOf(SessionState(""))}, {"Retired", reflect.TypeOf(false)},
		{"AdapterState", reflect.TypeOf(json.RawMessage(nil))},
	}
	typeOf := reflect.TypeOf(SessionView{})
	if typeOf.NumField() != len(want) {
		t.Fatalf("SessionView has %d fields; only the six credential-free fields are allowed", typeOf.NumField())
	}
	for i, expected := range want {
		field := typeOf.Field(i)
		if field.Name != expected.name || field.Type != expected.typeOf || field.Anonymous || field.Tag != "" {
			t.Fatalf("SessionView field %d=%+v: credential-free name, type and untagged shape changed", i, field)
		}
	}
}

const diagnosticReceiveZeroJSON = `{"idle_wake":false,"delivery":"","inbox":"","busy_delivery":"","lifecycle_hooks":false,"hook_delivery":false,"requires_listener":false,"requires_endpoint":false,"experimental":false}`
const diagnosticZeroJSON = `{"id":"","kind":"","state":"","retired":false,"liveness":"","updated_at":"","transport_health":"","hook_build_status":"","receive":` + diagnosticReceiveZeroJSON + `}`

func TestSessionDiagnosticJSONOmissions(t *testing.T) {
	for _, test := range []struct {
		name  string
		value SessionDiagnostic
		want  string
	}{
		{"zero", SessionDiagnostic{}, diagnosticZeroJSON},
		{"nil owner", SessionDiagnostic{Owner: nil}, diagnosticZeroJSON},
		{"zero owner pointer", SessionDiagnostic{Owner: &ProcessRef{}}, `{"id":"","kind":"","state":"","retired":false,"liveness":"","owner":{"pid":0,"start":"","role":""},"updated_at":"","transport_health":"","hook_build_status":"","receive":` + diagnosticReceiveZeroJSON + `}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := json.Marshal(test.value)
			if err != nil || string(got) != test.want {
				t.Fatalf("diagnostic=%s, error=%v, want %s", got, err, test.want)
			}
		})
	}
}

func TestSessionDiagnosticFullJSON(t *testing.T) {
	value := SessionDiagnostic{ID: "cursor:id", Kind: "cursor", Name: "Name", State: "retired", Retired: true, RetiredReason: "done", Liveness: "exited", Owner: &ProcessRef{PID: 7, Start: "start", Role: "client"}, UpdatedAt: "updated", TransportHealth: "transport", ListenerHealth: "listener", HookBuild: "build", HookBuildStatus: "current", RecoveryHint: "hint", Receive: ReceiveCapabilities{IdleWake: true, Delivery: "fixture", Inbox: "journal", BusyDelivery: "next_hook", LifecycleHooks: true, HookDelivery: true, RequiresListener: true, RequiresEndpoint: true, Experimental: true}}
	const want = `{"id":"cursor:id","kind":"cursor","name":"Name","state":"retired","retired":true,"retired_reason":"done","liveness":"exited","owner":{"pid":7,"start":"start","role":"client"},"updated_at":"updated","transport_health":"transport","listener_health":"listener","hook_build":"build","hook_build_status":"current","recovery_hint":"hint","receive":{"idle_wake":true,"delivery":"fixture","inbox":"journal","busy_delivery":"next_hook","lifecycle_hooks":true,"hook_delivery":true,"requires_listener":true,"requires_endpoint":true,"experimental":true}}`
	got, err := json.Marshal(value)
	if err != nil || string(got) != want {
		t.Fatalf("full diagnostic=%s, error=%v, want %s", got, err, want)
	}
	var decoded SessionDiagnostic
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatal(err)
	}
	again, err := json.Marshal(decoded)
	if err != nil || string(again) != want {
		t.Fatalf("roundtrip=%s, error=%v, want %s", again, err, want)
	}
}
