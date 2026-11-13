package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func TestPluginOwnerBinaryRoundtrip(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	store, session := pluginOwnerFixture(t)
	state := store.Dir
	assertPluginOwnerDisabled(t, binary, state)
	for _, args := range [][]string{{"add", "fake-notifier", "--exec", feedFixture(t)}, {"configure", "fake-notifier", "--scope", "all"}, {"grant", "fake-notifier", "--status"}, {"enable", "fake-notifier"}} {
		if output, err := pluginOwnerCLI(t, binary, state, args...); err != nil {
			t.Fatal(args, string(output), err)
		}
	}
	assertPluginOwnerStatus(t, binary, state, true, true)
	first, firstDone := startPluginOwnerHost(t, binary, state)
	assertPluginOwnerStatus(t, binary, state, true, false)
	outbox := filepath.Join(state, "plugins", "fake-notifier.data", "events")
	pluginOwnerEventually(t, func() bool { _, err := os.Stat(outbox); return err == nil })
	if files, err := os.ReadDir(outbox); err != nil || len(files) != 0 {
		t.Fatal("enable gap replayed historical notifications", files, err)
	}
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	eventID := awaitPluginOwnerEvent(t, outbox, session.ID)
	if err := first.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitPluginOwnerHost(t, first, firstDone)
	second, secondDone := startPluginOwnerHost(t, binary, state)
	if files, err := os.ReadDir(outbox); err != nil || len(files) != 1 || files[0].Name() != eventID+".json" {
		t.Fatal("restart duplicated the notification", files, err)
	}
	if output, err := pluginOwnerCLI(t, binary, state, "disable", "fake-notifier"); err != nil {
		t.Fatal(string(output), err)
	}
	waitPluginOwnerHost(t, second, secondDone)
	assertPluginOwnerLeaseReleased(t, state)
	assertPluginOwnerStatus(t, binary, state, false, false)
}

func assertPluginOwnerLeaseReleased(t *testing.T, state string) {
	t.Helper()
	fd, err := journal.OpenLockFile(filepath.Join(state, "plugins", "fake-notifier.lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	syscall.CloseOnExec(fd)
	t.Cleanup(func() { _ = journal.CloseLockFile(fd) })
	if err := journal.LockExclusive(fd, true); err != nil {
		t.Fatal("disabled host left a child holding its execution lease", err)
	}
}

func pluginOwnerFixture(t *testing.T) (bus.Store, model.Session) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	store, err := bus.NewStore(state, adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := store.Register("codex", "00000000-0000-4000-8000-000000000001", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return store, session
}

func assertPluginOwnerDisabled(t *testing.T, binary, state string) {
	t.Helper()
	output, err := pluginOwnerCLI(t, binary, state, "run", "fake-notifier")
	if err == nil || !strings.Contains(string(output), "disabled") {
		t.Fatal("disabled plugin executed", string(output), err)
	}
	if _, err := os.Stat(filepath.Join(state, "plugins", "fake-notifier.lock")); !os.IsNotExist(err) {
		t.Fatal("disabled run acquired a host lease", err)
	}
}

func awaitPluginOwnerEvent(t *testing.T, outbox, session string) string {
	t.Helper()
	var event model.Event
	pluginOwnerEventually(t, func() bool {
		files, err := os.ReadDir(outbox)
		if err != nil || len(files) != 1 {
			return false
		}
		var ok bool
		event, ok = fixtureEvent(filepath.Join(outbox, files[0].Name()))
		return ok && event.Validate() == nil &&
			files[0].Name() == event.ID+".json" && event.Session.ID == session && event.Session.State == model.StateBusy
	})
	return event.ID
}

func pluginOwnerCLI(t *testing.T, binary, state string, args ...string) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, append([]string{"--state", state, "--log-level", "off", "plugins"}, args...)...)
	if len(args) > 0 && (args[0] == "grant" || args[0] == "configure") {
		master, slavePath, err := openTestPTY()
		if err != nil {
			t.Fatal("owner terminal fixture", err)
		}
		defer master.Close()
		slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer slave.Close()
		cmd.Stdin = slave
	}
	return cmd.CombinedOutput()
}

func assertPluginOwnerStatus(t *testing.T, binary, state string, enabled, gap bool) {
	t.Helper()
	raw, err := pluginOwnerCLI(t, binary, state, "status", "fake-notifier")
	var status struct {
		pluginfeed.PluginFeedStatus
	}
	if err != nil || json.Unmarshal(raw, &status) != nil {
		t.Fatal("status command failed", string(raw), err)
	}
	if status.Enabled != enabled || status.GapPending != gap || status.LastGap != "enabled" ||
		status.Scope != "all" || !status.Access.Status || status.Launch == nil {
		t.Fatal("status misrepresented the owner profile/gap", string(raw))
	}
}

func startPluginOwnerHost(t *testing.T, binary, state string) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	log, err := os.CreateTemp(t.TempDir(), "host-")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), binary, "--state", state, "--log-level", "off", "plugins", "run", "fake-notifier")
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		stopPluginOwnerHost(t, cmd, done, state)
		_ = log.Close()
	})
	pluginOwnerEventually(t, func() bool {
		select {
		case <-done:
			raw, _ := os.ReadFile(log.Name())
			t.Fatal("host exited before readiness", string(raw))
		default:
		}
		raw, err := os.ReadFile(log.Name())
		return err == nil && strings.Contains(string(raw), "fake-notifier ready")
	})
	return cmd, done
}

func stopPluginOwnerHost(t *testing.T, cmd *exec.Cmd, done <-chan struct{}, state string) {
	t.Helper()
	select {
	case <-done:
	default:
		_ = cmd.Process.Kill()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("cleanup could not reap the plugin host")
			return
		}
	}
	pluginOwnerEventually(t, func() bool { return pluginOwnerLeaseAvailable(t, state) })
}

func pluginOwnerLeaseAvailable(t *testing.T, state string) bool {
	t.Helper()
	fd, err := journal.OpenLockFile(filepath.Join(state, "plugins", "fake-notifier.lock"), false)
	if errors.Is(err, os.ErrNotExist) {
		return true // A reaped parent that never created its lease could not spawn a child.
	}
	if err != nil {
		t.Fatal("inspect plugin cleanup lease", err)
	}
	syscall.CloseOnExec(fd)
	lockErr := journal.LockExclusive(fd, true)
	if err := journal.CloseLockFile(fd); err != nil {
		t.Fatal("close cleanup lease", err)
	}
	if lockErr == nil {
		return true
	}
	if !errors.Is(lockErr, syscall.EWOULDBLOCK) && !errors.Is(lockErr, syscall.EAGAIN) {
		t.Fatal("inspect plugin cleanup lock", lockErr)
	}
	return false
}

func TestPluginOwnerForcedParentCleanupReleasesChildLease(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	store, _ := pluginOwnerFixture(t)
	for _, args := range [][]string{{"add", "fake-notifier", "--exec", feedFixture(t)}, {"configure", "fake-notifier", "--scope", "all"}, {"grant", "fake-notifier", "--status"}, {"enable", "fake-notifier"}} {
		if raw, err := pluginOwnerCLI(t, binary, store.Dir, args...); err != nil {
			t.Fatal("prepare cleanup host", string(raw), err)
		}
	}
	cmd, done := startPluginOwnerHost(t, binary, store.Dir)
	stopPluginOwnerHost(t, cmd, done, store.Dir)
	assertPluginOwnerLeaseReleased(t, store.Dir)
}

func TestPluginOwnerCleanupLeaseProbeHonorsContention(t *testing.T) {
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "plugins"), 0700); err != nil {
		t.Fatal(err)
	}
	fd, err := journal.OpenLockFile(filepath.Join(state, "plugins", "fake-notifier.lock"), true)
	if err != nil {
		t.Fatal(err)
	}
	syscall.CloseOnExec(fd)
	defer func() { _ = journal.CloseLockFile(fd) }()
	if err := journal.LockExclusive(fd, true); err != nil {
		t.Fatal(err)
	}
	if pluginOwnerLeaseAvailable(t, state) {
		t.Fatal("cleanup lease probe accepted a held child lease")
	}
	if err := journal.Unlock(fd); err != nil {
		t.Fatal(err)
	}
	if !pluginOwnerLeaseAvailable(t, state) {
		t.Fatal("cleanup lease probe refused a released lease")
	}
}

func waitPluginOwnerHost(t *testing.T, cmd *exec.Cmd, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		if !cmd.ProcessState.Success() {
			t.Fatal("host did not shut down cleanly", cmd.ProcessState)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("host/child did not exit")
	}
}

func pluginOwnerEventually(t *testing.T, ready func() bool) {
	t.Helper()
	until := time.Now().Add(10 * time.Second)
	for time.Now().Before(until) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out waiting for local plugin state")
}

func pluginOwnerLocal(t *testing.T, state string, args ...string) ([]byte, error) {
	t.Helper()
	previous := pluginAccessTerminal
	pluginAccessTerminal = func(io.Reader) bool { return true }
	defer func() { pluginAccessTerminal = previous }()
	cmd := newRootCommand(t.TempDir())
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs(append([]string{"--state", state, "--log-level", "off", "plugins"}, args...))
	err := cmd.ExecuteContext(t.Context())
	return output.Bytes(), err
}

func TestPluginOwnerConfigureValidatesScopeAndEnabledState(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	if raw, err := pluginOwnerLocal(t, store.Dir, "add", "fake-notifier", "--exec", feedFixture(t)); err != nil {
		t.Fatal(string(raw), err)
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "configure", "fake-notifier", "--scope", "none"); err != nil {
		t.Fatal("scope none was refused", string(raw), err)
	}
	status, err := pluginfeed.NewPluginFeed(store, "fake-notifier").PluginStatus()
	if err != nil || status.Enabled || status.Scope != "none" {
		t.Fatal("configure did not store the scope", status, err)
	}
	for _, args := range [][]string{{"--scope", "everything"}, {"--scope", "ALL"}, {}, {"--session", "codex:00000000-0000-4000-8000-000000000002"}} {
		if raw, err := pluginOwnerLocal(t, store.Dir, append([]string{"configure", "fake-notifier"}, args...)...); err == nil {
			t.Fatal("configure accepted", args, string(raw))
		}
	}
	for _, args := range [][]string{{"configure", "fake-notifier", "--scope", "all"}, {"grant", "fake-notifier", "--status"}, {"enable", "fake-notifier"}} {
		if raw, err := pluginOwnerLocal(t, store.Dir, args...); err != nil {
			t.Fatal(args, string(raw), err)
		}
	}
	if raw, err := pluginOwnerLocal(t, store.Dir, "configure", "fake-notifier", "--scope", "none"); err != nil {
		t.Fatal("a scope change on an enabled plugin did not rebind", string(raw), err)
	}
	if status, err := pluginfeed.NewPluginFeed(store, "fake-notifier").PluginStatus(); err != nil || !status.Enabled || status.Scope != "none" {
		t.Fatal("rebind lost the enabled state or the scope", status, err)
	}
}

func TestPluginOwnerStatusDoesNotClaimAHostIsRunning(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	for _, args := range [][]string{{"add", "fake-notifier", "--exec", feedFixture(t)}, {"configure", "fake-notifier", "--scope", "all"}, {"grant", "fake-notifier", "--status"}, {"enable", "fake-notifier"}} {
		if raw, err := pluginOwnerLocal(t, store.Dir, args...); err != nil {
			t.Fatal(string(raw), err)
		}
	}
	raw, err := pluginOwnerLocal(t, store.Dir, "status", "fake-notifier")
	var status map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &status) != nil {
		t.Fatal("status failed", string(raw), err)
	}
	wanted := []string{"access", "data_dir", "enabled", "epoch", "gap_pending", "id", "last_gap", "launch", "pin_matches", "plugin", "revision", "scope"}
	keys := make([]string, 0, len(status))
	for key := range status {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, wanted) || string(status["enabled"]) != "true" || string(status["gap_pending"]) != "true" {
		t.Fatal("status inferred runtime state or omitted configuration", string(raw))
	}
	for _, name := range []string{"fake-notifier.data/events"} {
		if _, err := os.Stat(filepath.Join(store.Dir, "plugins", name)); !os.IsNotExist(err) {
			t.Fatal("owner command launched or inspected a running host", name, err)
		}
	}
}
