package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func identityOf(t *testing.T, path string) fileIdentity {
	t.Helper()
	pinned, err := pinnedIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	return pinned
}

func openTestLease(t *testing.T) *os.File {
	t.Helper()
	lease, err := pluginfeed.OpenPluginLease(filepath.Join(t.TempDir(), "lease"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	return lease
}

// S03: a manual plugin child gets exactly HOME and the fixed PATH; agent
// selectors, capabilities, loader and runtime variables and the parent's
// PATH are not passed on.
func TestPluginManualRuntimeEnvironment(t *testing.T) {
	for key, value := range map[string]string{
		"ZALKI_ACTOR": "claude:x", "ZALKI_CAPABILITY": "secret", "ZALKI_STATE": "/elsewhere",
		"GODEBUG": "inittrace=1", "DYLD_LIBRARY_PATH": "/nonexistent", "PATH": "/inherited:" + os.Getenv("PATH"),
	} {
		t.Setenv(key, value)
	}
	home := t.TempDir()
	c, err := launchChild(t.Context(), os.Args[0], identityOf(t, os.Args[0]), []string{"-test.run=^TestPluginEnvChild$"}, home, home, openTestLease(t))
	if err != nil {
		t.Fatal(err)
	}
	defer c.stop(nil)
	line, err := c.reader.ReadString('\n')
	var env []string
	if err != nil || json.Unmarshal([]byte(line), &env) != nil {
		// Only the size: the report holds environment values.
		t.Fatalf("no environment report: %d bytes, read error %v", len(line), err)
	}
	checkChildEnvironment(t, env, home)
}

// checkChildEnvironment reports only variable NAMES, never values: a
// failure must not copy an inherited secret into a test log.
func checkChildEnvironment(t *testing.T, env []string, home string) {
	t.Helper()
	want := map[string]string{"HOME": home, "PATH": home + "/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"}
	var unexpected, wrong []string
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		switch fixed, ok := want[name]; {
		case !ok:
			unexpected = append(unexpected, name)
		case value != fixed:
			wrong = append(wrong, name)
		}
	}
	slices.Sort(unexpected)
	if len(unexpected) > 0 || len(wrong) > 0 || len(env) != len(want) {
		t.Fatalf("child environment: unexpected variables %q, not the fixed value %q, %d entries (want HOME and PATH only)", unexpected, wrong, len(env))
	}
}

func TestPluginEnvChild(t *testing.T) {
	if len(os.Args) != 2 || os.Args[1] != "-test.run=^TestPluginEnvChild$" {
		return
	}
	raw, _ := json.Marshal(os.Environ())
	fmt.Printf("%s\n", raw)
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestPluginHomeMustBeCleanAbsolute(t *testing.T) {
	for _, home := range []string{"relative", "/a/../b", "/a/"} {
		if _, err := launchChild(t.Context(), os.Args[0], identityOf(t, os.Args[0]), nil, t.TempDir(), home, openTestLease(t)); err == nil || !strings.Contains(err.Error(), "clean absolute") {
			t.Errorf("home %q: %v", home, err)
		}
	}
}

// The script stays the process (no exec), so its path is in the process
// list while it runs: that is what runningFile looks for.
var pinFirst, pinSecond = []byte("#!/bin/sh\ncat #A\n"), []byte("#!/bin/sh\ncat #B\n")

// runningFile reports whether a process still runs the script at path. It
// is independent of launchChild's own result.
func runningFile(t *testing.T, path string) bool {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), "pgrep", "-f", path).Output()
	return err == nil && len(bytes.TrimSpace(out)) > 0
}

func replaceSameSize(t *testing.T, path string) {
	t.Helper()
	writeExecutable(t, path+".new", pinSecond)
	if err := os.Rename(path+".new", path); err != nil {
		t.Fatal(err)
	}
}

func rewriteInPlace(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err == nil {
		_, err = file.WriteAt(pinSecond, 0)
		err = joinClose(err, file)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func changeMode(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0500); err != nil {
		t.Fatal(err)
	}
}

// S16: the identity read before the digest check must still hold once the
// child has started. A same-size replacement, an in-place rewrite or a mode
// change in that window stops and reaps the child with a permanent reason.
func TestPluginPinIdentityAfterStart(t *testing.T) {
	for name, change := range map[string]func(*testing.T, string){
		"same-size replacement": replaceSameSize,
		"in-place rewrite":      rewriteInPlace,
		"mode change":           changeMode,
	} {
		t.Run(name, func(t *testing.T) { checkChangedPinRefused(t, change) })
	}
	t.Run("unchanged", checkUnchangedPinStarts)
	t.Run("link", checkLinkedPinRefused)
}

func checkChangedPinRefused(t *testing.T, change func(*testing.T, string)) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin")
	writeExecutable(t, path, pinFirst)
	pinned := identityOf(t, path)
	time.Sleep(20 * time.Millisecond) // a later change time
	change(t, path)
	c, err := launchChild(t.Context(), path, pinned, nil, t.TempDir(), "", openTestLease(t))
	if c != nil || model.ErrorCodeOf(err) != model.CodeConflict || !strings.HasSuffix(err.Error(), "the plugin file changed while it started; install it again") {
		t.Fatalf("a changed file started: %v", err)
	}
	if runningFile(t, path) {
		t.Fatal("a refused changed file is still running")
	}
}

func checkUnchangedPinStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin")
	writeExecutable(t, path, pinFirst)
	c, err := launchChild(t.Context(), path, identityOf(t, path), nil, t.TempDir(), "", openTestLease(t))
	if err != nil {
		t.Fatal("an unchanged pin did not start", err)
	}
	if !runningFile(t, path) {
		t.Fatal("the process check did not see a running plugin")
	}
	if err := c.stop(nil); err != nil {
		t.Fatal(err)
	}
}

func checkLinkedPinRefused(t *testing.T) {
	dir := t.TempDir()
	writeExecutable(t, filepath.Join(dir, "plugin"), pinFirst)
	if err := os.Symlink(filepath.Join(dir, "plugin"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := pinnedIdentity(filepath.Join(dir, "link")); err == nil || !strings.Contains(err.Error(), "plugin launch changed") {
		t.Fatal("a linked executable was accepted", err)
	}
}

func writeExecutable(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.WriteFile(path, content, 0700); err != nil {
		t.Fatal(err)
	}
}

func joinClose(err error, file *os.File) error {
	if closeErr := file.Close(); err == nil {
		return closeErr
	}
	return err
}
