package install

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

// coopHelperEnv makes the test binary act as the cooperating process: it
// runs one uninstall of the item in the given home and pauses right after
// its bootout until the parent lets it go on.
const coopHelperEnv = "ZALKI_LOGIN_ITEM_COOP_HELPER"

func TestMain(m *testing.M) {
	if dir := os.Getenv(coopHelperEnv); dir != "" {
		code := runCoopHelper(dir, os.Getenv(coopHelperEnv+"_HOME"))
		_ = os.WriteFile(filepath.Join(dir, "done"), []byte{byte('0' + code)}, 0o600)
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// runCoopHelper is the second process. It has its own fake launchd (the
// job is loaded and ours); only the files and the operation lock are shared.
func runCoopHelper(dir, home string) int {
	state := filepath.Join(home, ".local", "state", "zalki")
	binary := "/opt/homebrew/bin/zalki"
	fake := &fakeLaunchd{loaded: true, program: binary, args: []string{binary, "plugins", "supervise", "--state", state}}
	// The pause is AFTER a known successful bootout: the wrapper runs the
	// fake first, then writes the phase witness and waits. The shared fake's
	// callback order is not changed.
	run := func(ctx context.Context, args ...string) ([]byte, int, error) {
		out, code, err := fake.run(ctx, args...)
		if len(args) > 0 && args[0] == "bootout" {
			coopPause(dir, fmt.Sprintf("bootout code=%d err=%t loaded=%t", code, err != nil, fake.loaded))
		}
		return out, code, err
	}
	accountHome = func() (string, error) { return home, nil }
	runLaunchctl, loginItemsSupported = run, true
	_, err := ManagedLoginItem(context.Background(), Request{Action: "uninstall", Binary: binary, StateDir: state, HomeDir: home, Apply: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 3
	}
	return 0
}

// coopWitness is the phase the child reports when it pauses: the bootout
// ran, launchd answered success, and the job is no longer loaded.
const coopWitness = "bootout code=0 err=false loaded=false"

// coopPause writes the phase witness and waits (at most 20 s) for resume.
// The witness appears whole: the parent reads it as soon as the file exists,
// so a plain write could be read while it was still empty.
func coopPause(dir, witness string) {
	pending := filepath.Join(dir, "paused.pending")
	_ = os.WriteFile(pending, []byte(witness), 0o600)
	_ = os.Rename(pending, filepath.Join(dir, "paused"))
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(dir, "resume")); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A real second process holds the operation lock across its bootout: this
// process's install is refused as busy with no file or launchctl change;
// after the other process finishes, a new authorized setup is created and
// kept. The child runs in its own process group with a hard time limit and
// is killed as a group from its owned handle in cleanup: no orphans and no
// real launchctl.
func TestPluginLoginItemCooperatingProcess(t *testing.T) {
	dir := t.TempDir()
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	child := startCoopChild(t, dir, f.home)

	waitForFile(t, filepath.Join(dir, "paused"), 15*time.Second)
	if witness, _ := os.ReadFile(filepath.Join(dir, "paused")); string(witness) != coopWitness {
		t.Fatalf("the child did not pause after a known successful bootout: %q", witness)
	}
	before := snapshotFiles(t, f.paths)
	f.fake.calls = nil
	if _, err := f.do("install", true); !errors.Is(err, errLoginItemBusy) {
		t.Fatalf("an install ran while another process held the operation: %v", err)
	}
	if after := snapshotFiles(t, f.paths); after != before || len(f.fake.mutations()) != 0 {
		t.Fatalf("a blocked install changed something: %v", f.fake.mutations())
	}

	if err := os.WriteFile(filepath.Join(dir, "resume"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitCoopChild(t, dir, child)
	if _, err := os.Lstat(f.paths.plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the cooperating uninstall left the plist")
	}

	// The later, authorized setup is new and is kept.
	f.fake.loaded, f.fake.calls = false, nil
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	var record loginItemRecord
	if _, err := readPrivateJSON(f.paths.record, &record); err != nil || record.Binary != f.binary || !slices.Contains(f.fake.mutations(), "bootstrap") {
		t.Fatalf("the later setup was not created and kept: %+v %v %v", record, err, f.fake.mutations())
	}
	if _, err := os.Lstat(f.paths.pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an operation stayed pending")
	}
}

// startCoopChild starts this test binary as the cooperating process in its
// own process group. Only waitCoopChild reaps it, so while ProcessState is
// nil the group still exists and cleanup may kill it from the owned handle;
// after reaping, nothing is signalled.
func startCoopChild(t *testing.T, dir, home string) *exec.Cmd {
	t.Helper()
	// Background: the context never cancels; the bounds are the done-mark
	// wait and the group kill in cleanup, both on the owned handle.
	child := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^$")
	child.Env = append(os.Environ(), coopHelperEnv+"="+dir, coopHelperEnv+"_HOME="+home)
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if child.ProcessState == nil { // not reaped: the group id is still ours
			_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
			_ = child.Wait()
		}
		_ = stderr.Close()
	})
	return child
}

// waitCoopChild waits (hard limit 25 s) for the child's done mark, then
// reaps it.
func waitCoopChild(t *testing.T, dir string, child *exec.Cmd) {
	t.Helper()
	waitForFile(t, filepath.Join(dir, "done"), 25*time.Second)
	err := child.Wait()
	mark, _ := os.ReadFile(filepath.Join(dir, "done"))
	if err != nil || string(mark) != "0" {
		raw, _ := os.ReadFile(filepath.Join(dir, "stderr"))
		t.Fatalf("the cooperating uninstall failed: %v %q %.4096s", err, mark, raw)
	}
}

func waitForFile(t *testing.T, path string, limit time.Duration) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s did not appear in %s", filepath.Base(path), limit)
}
