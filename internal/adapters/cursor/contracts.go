package cursor

import (
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
)

// Check every extension this adapter opts into; absent interfaces keep core defaults.
var (
	_ bus.Adapter               = Adapter{}
	_ agent.NameAdapter         = Adapter{}
	_ agent.HookAdapter         = Adapter{}
	_ agent.HealthAdapter       = Adapter{}
	_ bus.ConfigurationAdapter  = Adapter{}
	_ agent.RecoveryHintAdapter = Adapter{}
	_ agent.SessionStateAdapter = Adapter{}
)

var _ agent.SessionStateMigrationAdapter = (*Adapter)(nil)
