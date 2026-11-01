// Package adapters composes the concrete clients into the registry injected by
// the CLI. Client implementations never import this composition package.
package adapters

import (
	"github.com/zalkilabs/zalki/internal/adapters/claude"
	"github.com/zalkilabs/zalki/internal/adapters/codex"
	"github.com/zalkilabs/zalki/internal/adapters/cursor"
	"github.com/zalkilabs/zalki/internal/bus"
)

// Builtin returns a registry of every compiled-in adapter.
func Builtin() *bus.Registry {
	r, err := bus.NewRegistry(codex.New(), claude.New(), cursor.New())
	if err != nil {
		panic(err)
	}
	return r
}
