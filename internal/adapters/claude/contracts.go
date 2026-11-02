package claude

import "github.com/zalkilabs/zalki/internal/bus"

// Check every extension this adapter opts into; absent interfaces keep core defaults.
var (
	_ bus.Adapter              = Adapter{}
	_ bus.NameAdapter          = Adapter{}
	_ bus.HookAdapter          = Adapter{}
	_ bus.HealthAdapter        = Adapter{}
	_ bus.ConfigurationAdapter = Adapter{}
	_ bus.LivenessAdapter      = Adapter{}
	_ bus.RecoveryHintAdapter  = Adapter{}
	_ bus.CompatibilityAdapter = Adapter{}
)
