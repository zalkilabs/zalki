package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/zalkilabs/zalki/internal/plugins"
)

// Validate both the supplied spelling and resolved target before reading. A
// safe alias may resolve to a control-bearing path or fail while naming one.
func (p *pluginInstaller) readSource(folder string) (plugins.InstallManifest, error) {
	if !printablePluginPath(folder) {
		return plugins.InstallManifest{}, errors.New("plugin source path contains unsupported control characters")
	}
	resolved, err := plugins.ResolveInstallSource(folder)
	if err != nil {
		return plugins.InstallManifest{}, pluginSourceError(err)
	}
	if !printablePluginPath(resolved) {
		return plugins.InstallManifest{}, errors.New("plugin source path contains unsupported control characters")
	}
	p.folder = resolved
	manifest, err := plugins.ReadInstallManifest(resolved)
	if err != nil {
		return manifest, pluginSourceError(err)
	}
	return manifest, nil
}

// Preserve the original error chain while choosing its display text. Precision
// zero suppresses only the wrapped error's text; %w still preserves errors.Is/As.
// Resolve/read return ordinary filesystem/manifest errors, not custom formatters.
func pluginSourceError(cause error) error {
	if cause == nil {
		return nil
	}
	message := cause.Error()
	if printablePluginPath(message) {
		return cause
	}
	return fmt.Errorf("%s%.0w", strconv.QuoteToASCII(message), cause)
}
