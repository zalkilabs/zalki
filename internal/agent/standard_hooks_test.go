package agent_test

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// What a client reads at session start is that client's own format. An adapter
// that only embeds StandardHooks must therefore not satisfy HookAdapter: it has
// to write its own StartupOutput, and no client format is inherited from core.
func TestStandardHooksAloneIsNotAHookAdapter(t *testing.T) {
	var embedded struct{ agent.StandardHooks }
	if _, ok := any(embedded).(agent.HookAdapter); ok {
		t.Fatal("an adapter that only embeds StandardHooks is a HookAdapter, so StandardHooks supplies a startup output again; it must have none, each adapter renders its own")
	}
}

func TestStandardHooksKeepsTheNeutralDefaults(t *testing.T) {
	var embedded struct {
		agent.StandardHooks
		startup
	}
	if _, ok := any(embedded).(agent.HookAdapter); !ok {
		t.Fatal("StandardHooks plus a StartupOutput must be a HookAdapter")
	}
}

// startup is an adapter's own StartupOutput.
type startup struct{}

func (startup) StartupOutput(agent.StartupContext) (any, error) { return map[string]string{}, nil }
