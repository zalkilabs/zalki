package main

import (
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/install"
)

// installDeclarations adapts the registry's neutral configuration declarations
// to the installer's execution specification. All five fields are converted by
// name. The JSON maps are passed on as they are, never copied: the installer
// reads them but does not modify them.
func installDeclarations(registry *agent.Registry, configFile string) install.Declare {
	return func(binary, state string) ([]install.ConfigFile, error) {
		declarations, err := registry.Configuration(binary, []string{"--config", configFile, "--state", state})
		if err != nil {
			return nil, err
		}
		files := make([]install.ConfigFile, 0, len(declarations))
		for _, declaration := range declarations {
			files = append(files, install.ConfigFile{
				Path:          declaration.Path,
				Draft:         declaration.Draft,
				JSON:          declaration.JSON,
				TOML:          declaration.TOML,
				ConfigRootEnv: declaration.ConfigRootEnv,
			})
		}
		return files, nil
	}
}
