// Package adapters composes the concrete clients into the registry injected by
// the CLI. Client implementations never import this composition package.
package adapters

import (
	"github.com/zalkilabs/zalki/internal/adapters/claude"
	"github.com/zalkilabs/zalki/internal/adapters/codex"
	"github.com/zalkilabs/zalki/internal/adapters/cursor"
	"github.com/zalkilabs/zalki/internal/agent"
)

// Builtin returns a registry of every compiled-in adapter. It carries no Codex
// settings and no client version, so it is for listing and configuration; the
// CLI builds its delivery registry with BuiltinWithCodex.
func Builtin() *agent.Registry {
	return BuiltinWithCodex(false, "", "")
}

// BuiltinWithCodex copies invocation-local queue policy into the Codex adapter.
// configFile is the resolved Zalki settings path used in refusal guidance.
// clientVersion is the Zalki build version that Codex receives when its
// app-server is initialized.
func BuiltinWithCodex(allow bool, configFile, clientVersion string) *agent.Registry {
	r, err := agent.NewRegistry(codex.New(allow, configFile).WithClientVersion(clientVersion), claude.New(), cursor.New())
	if err != nil {
		panic(err)
	}
	return r
}
