package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Private operation seams let tests distinguish failures before and after the
// commit point without product flags or changes to the public API.
var renameConfig = os.Rename
var syncConfigDir = syncDirectory

func replaceConfig(path string, raw []byte) error {
	if err := makeConfigDir(filepath.Dir(path)); err != nil {
		return configReplaceError(path, err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".zalki-config-")
	if err != nil {
		return configReplaceError(path, err)
	}
	defer os.Remove(temp.Name()) //nolint:errcheck // Rename removes the temp name; failed writes are cleaned up.
	if err := writeConfigTemp(temp, raw); err != nil {
		return configReplaceError(path, err)
	}
	if err := renameConfig(temp.Name(), path); err != nil {
		return configReplaceError(path, err)
	}
	if err := syncConfigDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("config file %q was replaced, but its directory could not be synced. Check zalki config show before retrying.", path) //nolint:revive // error-strings: the exact CLI diagnostic contract includes a final period.
	}
	return nil
}

func writeConfigTemp(file *os.File, raw []byte) error {
	// Chmod makes the published mode exact even under a restrictive umask.
	err := file.Chmod(0600)
	if err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closed := file.Close()
	if err != nil {
		return err
	}
	return closed
}

func configReplaceError(path string, err error) error {
	return fmt.Errorf("config file %q could not be replaced: %w. The previous file was not changed.", path, err) //nolint:revive // error-strings: the exact CLI diagnostic contract includes a final period.
}

func syncDirectory(path string) error {
	dir, err := os.Open(path) //nolint:gosec // G304: selected absolute config directory.
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // Sync reports durability errors before descriptor cleanup.
	return dir.Sync()
}

// makeConfigDir sets only directories this invocation creates to 0700, before
// descending into them. Existing ancestors keep their owner's permissions.
func makeConfigDir(path string) error {
	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("config parent %q is not a directory", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := makeConfigDir(filepath.Dir(path)); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return err
	}
	return os.Chmod(path, 0700) //nolint:gosec // G302: private directory created by this invocation.
}
