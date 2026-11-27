package agent

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
)

// ConfigurationAdapter declares the client integration files used by installation.
type ConfigurationAdapter interface {
	Configuration(string, []string) []model.ConfigDeclaration
}

// Configuration returns only paths declared by the registered adapters. Each
// adapter receives its own copy of the caller's literal command arguments.
func (r *Registry) Configuration(binary string, args []string) ([]model.ConfigDeclaration, error) {
	if r == nil {
		return nil, errNoRegistry
	}
	if !filepath.IsAbs(binary) {
		return nil, errors.New("binary path must be absolute")
	}
	files := []model.ConfigDeclaration{}
	paths, drafts := map[string]bool{}, map[string]bool{"targets.json": true}
	for _, kind := range r.Kinds() {
		adapter := r.adapters[kind]
		configuration, ok := adapter.(ConfigurationAdapter)
		if !ok {
			continue
		}
		for _, file := range configuration.Configuration(binary, append([]string{}, args...)) {
			if err := validateConfigurationDeclaration(kind, file, paths, drafts); err != nil {
				return nil, err
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

func validateConfigurationDeclaration(kind string, file model.ConfigDeclaration, paths, drafts map[string]bool) error {
	if !fsutil.SafeRelativePath(file.Path) || !fsutil.SafeRelativePath(file.Draft) || filepath.Base(file.Draft) != file.Draft {
		return fmt.Errorf("%s declares an unsafe configuration path", kind)
	}
	if paths[file.Path] || drafts[file.Draft] {
		return errors.New("agent configuration paths and draft names must be unique")
	}
	if (file.JSON == nil) == (file.TOML == "") {
		return errors.New("configuration must declare exactly one JSON patch or TOML fragment")
	}
	if file.JSON != nil && len(file.JSON) == 0 {
		return errors.New("configuration JSON patch must not be empty")
	}
	return validateOwnedEntryDeclaration(kind, file)
}

// validateOwnedEntryDeclaration checks a declared owned entry: the file is JSON
// and the patch holds exactly that key, with an object value that is not empty.
func validateOwnedEntryDeclaration(kind string, file model.ConfigDeclaration) error {
	if file.OwnedEntry == "" {
		return nil
	}
	entry, isObject := file.JSON[file.OwnedEntry].(map[string]any)
	if !model.ValidName(file.OwnedEntry) || len(file.JSON) != 1 || !isObject || len(entry) == 0 {
		return fmt.Errorf("%s declares the owned entry %q, which needs a JSON patch with exactly that key and a non-empty object as its value", kind, file.OwnedEntry)
	}
	return nil
}
