package bus

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/zalkilabs/zalki/internal/install"
)

type configurationAdapter interface {
	Configuration(string, []string) []install.ConfigFile
}

// Configuration returns only paths declared by the registered adapters.
func (r *Registry) Configuration(binary, state string) ([]install.ConfigFile, error) {
	if r == nil {
		return nil, errNoRegistry
	}
	if !filepath.IsAbs(binary) || !filepath.IsAbs(state) {
		return nil, errors.New("binary and state paths must be absolute")
	}
	args := []string{"--state", state}
	files := []install.ConfigFile{}
	paths, drafts := map[string]bool{}, map[string]bool{"targets.json": true}
	for _, kind := range r.Kinds() {
		adapter := r.adapters[kind]
		configuration, ok := adapter.(configurationAdapter)
		if !ok {
			continue
		}
		for _, file := range configuration.Configuration(binary, append([]string{}, args...)) {
			if !install.SafePath(file.Path) || !install.SafePath(file.Draft) || filepath.Base(file.Draft) != file.Draft {
				return nil, fmt.Errorf("%s declares an unsafe configuration path", kind)
			}
			if paths[file.Path] || drafts[file.Draft] {
				return nil, errors.New("agent configuration paths and draft names must be unique")
			}
			if (file.JSON == nil) == (file.TOML == "") {
				return nil, errors.New("configuration must declare exactly one JSON patch or TOML fragment")
			}
			if file.JSON != nil && len(file.JSON) == 0 {
				return nil, errors.New("configuration JSON patch must not be empty")
			}
			paths[file.Path], drafts[file.Draft] = true, true
			files = append(files, file)
		}
	}
	if len(files) == 0 {
		return nil, errors.New("no adapter declares installation configuration")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
