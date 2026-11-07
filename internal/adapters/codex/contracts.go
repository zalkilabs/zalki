package codex

import (
	"github.com/zalkilabs/zalki/internal/agent"
)

// Check every extension this adapter opts into; absent interfaces keep core defaults.
var (
	_ agent.Adapter              = Adapter{}
	_ agent.NameAdapter          = Adapter{}
	_ agent.HookAdapter          = Adapter{}
	_ agent.HealthAdapter        = Adapter{}
	_ agent.ConfigurationAdapter = Adapter{}
	_ agent.LivenessAdapter      = Adapter{}
)
