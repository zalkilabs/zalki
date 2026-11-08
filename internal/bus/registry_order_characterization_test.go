package bus

import (
	"context"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// registryOrderAdapter records only the callbacks NewRegistry may invoke.
// It deliberately lacks HookAdapter so the optional interface check is visible.
type registryOrderAdapter struct {
	tag        string
	descriptor agent.AdapterDescriptor
	caps       model.ReceiveCapabilities
	calls      *[]string
	inputs     *[]model.Session
}

func (a registryOrderAdapter) Descriptor() agent.AdapterDescriptor {
	*a.calls = append(*a.calls, a.tag+".descriptor")
	return a.descriptor
}

func (a registryOrderAdapter) ReceiveCapabilities(v model.Session) model.ReceiveCapabilities {
	*a.calls = append(*a.calls, a.tag+".capabilities")
	*a.inputs = append(*a.inputs, v)
	return a.caps
}

func (registryOrderAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	panic("registry construction must not deliver")
}

type registryOrderHooks struct {
	registryOrderAdapter
	agent.StandardHooks
}

// Characterized, not endorsed: the first failing registry check decides the
// error and whether later adapter callbacks run. Hook maps have one entry so
// these cases do not impose an order on map iteration.
func TestRegistryErrorAndCallbackPrecedence(t *testing.T) {
	for _, test := range []struct {
		name      string
		kind      string
		label     string
		hooks     map[string]string
		caps      model.ReceiveCapabilities
		withHooks bool
		want      string
	}{
		{"kind before capabilities", "../probe", "Probe", nil, model.ReceiveCapabilities{}, false,
			"invalid agent kind or display label"},
		{"label before capabilities", "probe", "", nil, model.ReceiveCapabilities{}, false,
			"invalid agent kind or display label"},
		{"declaration before hook consistency", "probe", "Probe", map[string]string{"start": "SessionStart"}, model.ReceiveCapabilities{}, true,
			"probe capabilities: delivery and inbox mode must be declared"},
		{"hook inbox before timing", "probe", "Probe", nil, model.ReceiveCapabilities{Delivery: "fixture", Inbox: model.InboxNative, HookDelivery: true}, false,
			"probe capabilities: hook delivery requires a journal inbox"},
		{"timing before descriptor consistency", "probe", "Probe", map[string]string{"start": "SessionStart"}, model.ReceiveCapabilities{Delivery: "fixture", Inbox: model.InboxNative}, true,
			"probe capabilities: busy delivery timing must be declared"},
		{"descriptor consistency before mapping", "probe", "Probe", map[string]string{"start": "unsupported"}, registryOrderCaps(false), true,
			"probe hook capabilities do not match its descriptor"},
		{"descriptor consistency before optional interface", "probe", "Probe", map[string]string{"start": "SessionStart"}, registryOrderCaps(false), false,
			"probe hook capabilities do not match its descriptor"},
		{"native name before optional interface", "probe", "Probe", map[string]string{"": "SessionStart"}, registryOrderCaps(true), false,
			"probe declares an unsupported lifecycle event"},
		{"mapping before optional interface", "probe", "Probe", map[string]string{"start": "unsupported"}, registryOrderCaps(true), false,
			"probe declares an unsupported lifecycle event"},
		{"hook interface after valid mapping", "probe", "Probe", map[string]string{"start": "SessionStart"}, registryOrderCaps(true), false,
			"probe has no lifecycle adapter"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls []string
			var inputs []model.Session
			probe := registryOrderAdapter{tag: "probe", descriptor: agent.AdapterDescriptor{Kind: test.kind, Label: test.label, Hooks: test.hooks}, caps: test.caps, calls: &calls, inputs: &inputs}
			var adapter agent.Adapter = probe
			if test.withHooks {
				adapter = registryOrderHooks{registryOrderAdapter: probe}
			}
			later := registryOrderAdapter{tag: "later", calls: &calls, inputs: &inputs}
			registry, err := agent.NewRegistry(adapter, later)
			if err == nil || err.Error() != test.want || registry != nil {
				t.Fatalf("registry=%v error=%v, want nil registry and %q", registry, err, test.want)
			}
			wantCalls := []string{"probe.descriptor"}
			if test.kind == "probe" && test.label != "" {
				wantCalls = append(wantCalls, "probe.capabilities")
			}
			assertRegistryOrder(t, calls, inputs, wantCalls)
		})
	}
}

func registryOrderCaps(hooks bool) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "fixture", Inbox: model.InboxNative, BusyDelivery: model.DeliveryNextTurn, LifecycleHooks: hooks}
}

func assertRegistryOrder(t *testing.T, calls []string, inputs []model.Session, want []string) {
	t.Helper()
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("callback order=%v, want %v", calls, want)
	}
	count := 0
	for _, call := range want {
		if call == "probe.capabilities" || call == "first.capabilities" || call == "second.capabilities" {
			count++
		}
	}
	if len(inputs) != count {
		t.Fatalf("capability callback count=%d, want %d", len(inputs), count)
	}
	for _, input := range inputs {
		if !reflect.DeepEqual(input, model.Session{}) {
			t.Fatalf("registry capabilities received a nonzero session: %+v", input)
		}
	}
}

// Characterized, not endorsed: nil and duplicate adapters stop construction
// before the remaining callbacks, even if those would report other faults.
func TestRegistryNilAndDuplicateStopCallbacks(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		name := "nil before later adapter"
		if duplicate {
			name = "duplicate before its invalid capabilities"
		}
		t.Run(name, func(t *testing.T) {
			var calls []string
			var inputs []model.Session
			first := registryOrderAdapter{tag: "first", descriptor: agent.AdapterDescriptor{Kind: "probe", Label: "Probe"}, caps: registryOrderCaps(false), calls: &calls, inputs: &inputs}
			second := registryOrderAdapter{tag: "second", descriptor: first.descriptor, calls: &calls, inputs: &inputs}
			adapters := []agent.Adapter{nil, first}
			want := "nil agent adapter"
			var wantCalls []string
			if duplicate {
				adapters = []agent.Adapter{first, second, first}
				want = `duplicate agent adapter "probe"`
				wantCalls = []string{"first.descriptor", "first.capabilities", "second.descriptor"}
			}
			registry, err := agent.NewRegistry(adapters...)
			if err == nil || err.Error() != want || registry != nil {
				t.Fatalf("registry=%v error=%v, want nil registry and %q", registry, err, want)
			}
			assertRegistryOrder(t, calls, inputs, wantCalls)
		})
	}
}

func TestRegistryCallbacksFollowInputOrderAndUseZeroSessions(t *testing.T) {
	var calls []string
	var inputs []model.Session
	first := registryOrderAdapter{tag: "first", descriptor: agent.AdapterDescriptor{Kind: "zeta", Label: "Zeta"}, caps: registryOrderCaps(false), calls: &calls, inputs: &inputs}
	second := registryOrderAdapter{tag: "second", descriptor: agent.AdapterDescriptor{Kind: "alpha", Label: "Alpha", Hooks: map[string]string{"start": "SessionStart"}}, caps: registryOrderCaps(true), calls: &calls, inputs: &inputs}
	registry, err := agent.NewRegistry(first, registryOrderHooks{registryOrderAdapter: second})
	if err != nil {
		t.Fatal(err)
	}
	assertRegistryOrder(t, calls, inputs, []string{"first.descriptor", "first.capabilities", "second.descriptor", "second.capabilities"})
	if got := registry.Kinds(); !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
		t.Fatalf("kinds=%v, want sorted names despite callback input order", got)
	}
}
