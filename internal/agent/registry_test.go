package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// keyAdapter is a hook adapter whose descriptor is set by each test.
type keyAdapter struct {
	agent.StandardHooks
	descriptor agent.AdapterDescriptor
}

func (a keyAdapter) Descriptor() agent.AdapterDescriptor { return a.descriptor }

func (a keyAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "test", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, LifecycleHooks: len(a.descriptor.Hooks) > 0}
}

func (keyAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}

func keyDescriptor(key string, hooks map[string]string) agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "keyed", Label: "Keyed", HookEventKey: key, Hooks: hooks}
}

var oneHook = map[string]string{"start": "SessionStart"}

func TestRegistryHookEventKey(t *testing.T) {
	tests := []struct {
		name    string
		adapter agent.Adapter
		failure string
	}{
		{"empty key is accepted here", keyAdapter{descriptor: keyDescriptor("", oneHook)}, ""},
		{"camelCase key with hooks", keyAdapter{descriptor: keyDescriptor("hookEventName", oneHook)}, ""},
		{"key without hooks", keyAdapter{descriptor: keyDescriptor("hookEventName", nil)}, "a hook event key but no hooks"},
		{"the name Claude, Codex and Cursor send", keyAdapter{descriptor: keyDescriptor("hook_event_name", oneHook)}, ""},
		{"control character", keyAdapter{descriptor: keyDescriptor("hook\nEvent", oneHook)}, "invalid hook event key"},
		{"padded key", keyAdapter{descriptor: keyDescriptor(" hookEventName", oneHook)}, "invalid hook event key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := agent.NewRegistry(test.adapter)
			switch {
			case test.failure == "" && err != nil:
				t.Fatalf("registry refused a valid adapter: %v", err)
			case test.failure != "" && (err == nil || !strings.Contains(err.Error(), test.failure)):
				t.Fatalf("registry error = %v, want it to contain %q", err, test.failure)
			}
		})
	}
}
