package install

import (
	"errors"
	"fmt"
	"path/filepath"
)

// This mistake guard excludes dangling/missing-parent and absent-file case
// aliases, old relocated/uninstalled targets, and concurrent inode changes.
func validateReservedTarget(target string, file ConfigFile, request Request) error {
	if err := validateBootstrapSelection(request); err != nil {
		return err
	}
	reserved := []struct{ selector, path, fix string }{
		{"--config", request.ConfigFile, "config file"},
		{"--state", request.StateDir, "state directory"},
		{"--state", filepath.Join(request.StateDir, "installation.json"), "state directory"},
	}
	for _, item := range reserved {
		if item.path == "" {
			continue
		}
		if !filepath.IsAbs(item.path) {
			return fmt.Errorf("%s requires an absolute path", item.selector)
		}
		same, err := sameTargetFile(target, item.path)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("%s points at a file Zalki edits for %s (%q); choose another %s", item.selector, file.Draft, item.path, item.fix)
		}
	}
	return nil
}

func validateBootstrapSelection(request Request) error {
	if request.ConfigFile != "" && !filepath.IsAbs(request.ConfigFile) {
		return errors.New("--config requires an absolute path")
	}
	if request.ConfigFile != "" {
		for _, statePath := range []string{request.StateDir, filepath.Join(request.StateDir, "installation.json")} {
			same, err := sameTargetFile(request.ConfigFile, statePath)
			if err != nil {
				return err
			}
			if same {
				return fmt.Errorf("--config points at Zalki state (%q); choose another config file", request.ConfigFile)
			}
		}
	}
	return nil
}
