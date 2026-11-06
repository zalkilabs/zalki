// These in-package tests pin compound values without importing bus or OS
// mechanisms. Literal JSON expectations are independent of reflected fields.
package model

import (
	"encoding/json"
	"testing"
)

func TestCompoundJSONSurfaces(t *testing.T) {
	for _, test := range []struct {
		name  string
		value any
		want  string
	}{
		{"zero process", ProcessRef{}, `{"pid":0,"start":"","role":""}`},
		{"full process", ProcessRef{PID: 7, Start: "fingerprint", Role: "client"}, `{"pid":7,"start":"fingerprint","role":"client"}`},
		{"nil process pointer", (*ProcessRef)(nil), `null`},
		{"zero endpoint", Endpoint{}, `{"path":"","device":0,"inode":0,"changed_ns":0}`},
		{"full endpoint", Endpoint{Path: "/socket", Device: 9223372036854775811, Inode: 9223372036854775813, ChangedNS: -7},
			`{"path":"/socket","device":9223372036854775811,"inode":9223372036854775813,"changed_ns":-7}`},
		{"nil endpoint pointer", (*Endpoint)(nil), `null`},
		{"zero lease", ListenerLease{}, `{"id":"","generation":"","endpoint":{"path":"","device":0,"inode":0,"changed_ns":0}}`},
		{"full lease", ListenerLease{ID: "lease", Generation: "generation", Endpoint: Endpoint{Path: "/socket", Device: 3, Inode: 5, ChangedNS: 7}},
			`{"id":"lease","generation":"generation","endpoint":{"path":"/socket","device":3,"inode":5,"changed_ns":7}}`},
		{"nil lease pointer", (*ListenerLease)(nil), `null`},
		{"zero capabilities", ReceiveCapabilities{}, `{"idle_wake":false,"delivery":"","inbox":"","busy_delivery":"","lifecycle_hooks":false,"hook_delivery":false,"requires_listener":false,"requires_endpoint":false,"experimental":false}`},
		{"full capabilities", ReceiveCapabilities{IdleWake: true, Delivery: "custom", Inbox: "journal", BusyDelivery: "next_hook", LifecycleHooks: true, HookDelivery: true, RequiresListener: true, RequiresEndpoint: true, Experimental: true},
			`{"idle_wake":true,"delivery":"custom","inbox":"journal","busy_delivery":"next_hook","lifecycle_hooks":true,"hook_delivery":true,"requires_listener":true,"requires_endpoint":true,"experimental":true}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := json.Marshal(test.value)
			if err != nil || string(got) != test.want {
				t.Fatalf("JSON=%s, error=%v; want %s", got, err, test.want)
			}
		})
	}
}

func TestListenerLeaseKeepsEndpointByValue(t *testing.T) {
	endpoint := Endpoint{Path: "/original", Device: 3, Inode: 5, ChangedNS: 7}
	lease := ListenerLease{ID: "lease", Generation: "generation", Endpoint: endpoint}
	endpoint.Path = "/changed"
	copyOfLease := lease
	copyOfLease.Endpoint.Path = "/other"
	if lease.Endpoint != (Endpoint{Path: "/original", Device: 3, Inode: 5, ChangedNS: 7}) {
		t.Fatalf("lease shared its endpoint storage: %+v", lease.Endpoint)
	}
}

func TestProcessAndListenerWireSpellings(t *testing.T) {
	for _, test := range []struct {
		name, value, want string
	}{
		{"LivenessAlive", string(LivenessAlive), "alive"},
		{"LivenessExited", string(LivenessExited), "exited"},
		{"LivenessUnknown", string(LivenessUnknown), "unknown"},
		{"ListenerNone", string(ListenerNone), "none"},
		{"ListenerRunning", string(ListenerRunning), "running"},
		{"ListenerStale", string(ListenerStale), "stale"},
		{"ListenerUnleased", string(ListenerUnleased), "unleased"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.value != test.want {
				t.Fatalf("%s=%q, want %q", test.name, test.value, test.want)
			}
		})
	}
}
