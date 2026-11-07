package codex

import (
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
)

// Check every extension this adapter opts into; absent interfaces keep core defaults.
var (
	_ bus.Adapter              = Adapter{}
	_ bus.NameAdapter          = Adapter{}
	_ agent.HookAdapter        = Adapter{}
	_ bus.HealthAdapter        = Adapter{}
	_ bus.ConfigurationAdapter = Adapter{}
	_ bus.LivenessAdapter      = Adapter{}
)
