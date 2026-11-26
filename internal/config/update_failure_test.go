package config

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestConfigEditAtomicFailure(t *testing.T) {
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, "{}\n")
	original := renameConfig
	t.Cleanup(func() { renameConfig = original })
	renameConfig = func(_, _ string) error { return errors.New("rename fixture failure") }
	value := "debug"
	_, err := Update(input, "logging.level", &value)
	raw, _ := os.ReadFile(path)
	entries, _ := os.ReadDir(filepath.Dir(path))
	if err == nil || !strings.Contains(err.Error(), "The previous file was not changed") || string(raw) != "{}\n" || len(entries) != 1 {
		t.Fatal("failed atomic replacement lost original or temp", err, string(raw), entries)
	}
}

func TestConfigEditSyncAfterRenameFailure(t *testing.T) {
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, "{}\n")
	original := syncConfigDir
	t.Cleanup(func() { syncConfigDir = original })
	syncConfigDir = func(string) error { return errors.New("sync fixture failure") }
	value := "debug"
	_, err := Update(input, "logging.level", &value)
	raw, _ := os.ReadFile(path)
	entries, _ := os.ReadDir(filepath.Dir(path))
	if err == nil || !strings.Contains(err.Error(), "was replaced, but its directory could not be synced") || !strings.Contains(string(raw), "debug") || len(entries) != 1 {
		t.Fatal("post-commit error untruthful", err, string(raw), entries)
	}
}

// The directory sync gets the directory of the config file, not the file.
func TestConfigEditSyncsTheConfigDirectory(t *testing.T) {
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, "{}\n")
	original := syncConfigDir
	t.Cleanup(func() { syncConfigDir = original })
	var synced []string
	syncConfigDir = func(dir string) error {
		synced = append(synced, dir)
		return original(dir)
	}
	value := "debug"
	if _, err := Update(input, "logging.level", &value); err != nil {
		t.Fatal(err)
	}
	if want := []string{filepath.Dir(path)}; !slices.Equal(synced, want) {
		t.Fatalf("synced %q, want %q", synced, want)
	}
}

// The default directory sync is a real sync: a missing directory is an error.
// Without this, a seam replaced by a no-op would pass every other test.
func TestConfigDirSyncSeamReallySyncs(t *testing.T) {
	if err := syncConfigDir(t.TempDir()); err != nil {
		t.Fatalf("sync of an existing directory: %v", err)
	}
	if err := syncConfigDir(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sync of a missing directory = %v, want os.ErrNotExist", err)
	}
}

func TestConfigEditAtomicReaders(t *testing.T) {
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, `{"logging":{"level":"info"}}`)
	stopped, done := make(chan struct{}), make(chan error, 1)
	go func() {
		for {
			select {
			case <-stopped:
				done <- nil
				return
			default:
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				done <- err
				return
			}
			if !strings.Contains(string(raw), `"logging"`) || !json.Valid(raw) {
				done <- errors.New("reader saw partial JSON")
				return
			}
		}
	}()
	for range 30 {
		editValue(t, input, "logging.level", "warn")
		editValue(t, input, "logging.level", "info")
	}
	close(stopped)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestConfigEditRefusesUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"mode", "directory", "symlink", "fifo", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			input := fixtureInput(t, nil)
			path := fixtureFile(t, input, "{}\n")
			switch kind {
			case "mode":
				os.Chmod(path, 0644)
			case "directory":
				os.Remove(path)
				os.Mkdir(path, 0700)
			case "symlink":
				os.Remove(path)
				os.Symlink(filepath.Join(input.Home, "absent"), path)
			case "fifo":
				os.Remove(path)
				if err := syscall.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				os.WriteFile(path, []byte(strings.Repeat(" ", maxConfigBytes+1)), 0600)
			}
			value := "debug"
			if _, err := Update(input, "logging.level", &value); err == nil {
				t.Fatal("unsafe file replaced")
			}
			info, err := os.Lstat(path)
			if err != nil || (kind == "symlink" && info.Mode()&os.ModeSymlink == 0) || (kind == "directory" && !info.IsDir()) {
				t.Fatal("unsafe path changed", info, err)
			}
		})
	}
}

func TestConfigEditSerializedBound(t *testing.T) {
	input := fixtureInput(t, nil)
	path := fixtureFile(t, input, "{}\n")
	value := strings.Repeat("x", maxConfigBytes)
	if _, err := Update(input, "logging.file", &value); err == nil || !strings.Contains(err.Error(), "65536") {
		t.Fatal("oversize candidate accepted", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "{}\n" {
		t.Fatal("oversize candidate published")
	}
}

func TestConfigPermissionRepairCommand(t *testing.T) {
	input := fixtureInput(t, nil)
	path := filepath.Join(input.Home, "space ' $(touch SENTINEL) `touch OTHER`.json")
	input.Flags["config"] = path
	fixtureFile(t, input, "{}\n")
	os.Chmod(path, 0644)
	_, err := Load(input)
	prefix := path + ": config permissions allow group or other access. Run "
	if err == nil || !strings.HasPrefix(err.Error(), prefix) {
		t.Fatal("missing exact repair", err)
	}
	command := strings.TrimPrefix(err.Error(), prefix)
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
	cmd.Dir = input.Home
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("repair not executable", err, string(raw))
	}
	info, _ := os.Stat(path)
	entries, _ := os.ReadDir(input.Home)
	if info.Mode().Perm() != 0600 || len(entries) != 1 {
		t.Fatal("repair targeted wrong path or ran substitution", entries)
	}
}
