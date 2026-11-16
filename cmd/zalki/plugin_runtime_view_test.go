package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/install"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

const fixtureStart = "1700000000.000001"

// liveHost makes the fresh checks see pid as this user's live host with
// fixtureStart, under a live controller; each field can be changed.
type liveHost struct {
	pid, uid     int
	start        string
	zombie, gone bool
	readFails    bool
	noController bool
}

func useHost(t *testing.T, h liveHost) {
	t.Helper()
	oldRead, oldLive, oldUID := readProcess, controllerLive, currentUID
	readProcess = func(pid int) (procwatch.Info, error) {
		switch {
		case h.gone:
			return procwatch.Info{}, syscall.ESRCH
		case h.readFails:
			return procwatch.Info{}, syscall.EIO
		}
		return procwatch.Info{PID: h.pid, UID: uint32(h.uid), Start: h.start, Zombie: h.zombie}, nil
	}
	controllerLive = func(string) bool { return !h.noController }
	currentUID = func() int { return 501 }
	t.Cleanup(func() { readProcess, controllerLive, currentUID = oldRead, oldLive, oldUID })
}

func runningRecord() pluginRunState {
	since := time.Date(2026, 11, 14, 8, 2, 0, 0, time.UTC)
	return pluginRunState{Schema: 1, Installation: "00000000-0000-4000-8000-000000000001", ControllerInstance: "00000000-0000-4000-8000-000000000002",
		State: runRunning, PID: 4242, PluginPID: 4243, HostStart: fixtureStart, Since: &since}
}

// "observed running" needs all five checks; each one false alone is never
// running (positive absence is stopped, everything else unknown).
func TestPluginRuntimeObservedRunning(t *testing.T) {
	good := liveHost{pid: 4242, uid: 501, start: fixtureStart}
	for _, tc := range []struct {
		name   string
		host   liveHost
		record func(*pluginRunState)
		want   string
	}{
		{"all five hold", good, nil, observedRunning},
		{"no controller lock", liveHost{pid: 4242, uid: 501, start: fixtureStart, noController: true}, nil, observedUnknown},
		{"pid 0", good, func(r *pluginRunState) { r.PID, r.HostStart, r.State = 0, "", runStopped }, observedStopped},
		{"other uid", liveHost{pid: 4242, uid: 502, start: fixtureStart}, nil, observedUnknown},
		{"zombie", liveHost{pid: 4242, uid: 501, start: fixtureStart, zombie: true}, nil, observedUnknown},
		{"reused pid (other start)", liveHost{pid: 4242, uid: 501, start: "1700000999.000001"}, nil, observedUnknown},
		{"no host_start (older file)", good, func(r *pluginRunState) { r.HostStart = "" }, observedUnknown},
		{"process gone", liveHost{gone: true}, nil, observedStopped},
		{"process unreadable", liveHost{readFails: true}, nil, observedUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useHost(t, tc.host)
			record := runningRecord()
			if tc.record != nil {
				tc.record(&record)
			}
			if got := observeRun("/state", record); got != tc.want {
				t.Fatalf("observed %q, want %q", got, tc.want)
			}
		})
	}
}

// The 7b record field: bounded, only with a PID, written with it and
// cleared with it; an older file without it stays valid and unchanged.
func TestPluginRunStateHostStart(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start string
		pid   int
		valid bool
	}{
		{"empty", "", 0, true}, {"darwin", fixtureStart, 1, true}, {"linux", "0f1e2d3c-aaaa-bbbb-cccc-111122223333:4567", 1, true},
		{"without pid", fixtureStart, 0, false}, {"too long", strings.Repeat("1", 65), 1, false}, {"raw text", "Tue Nov 14", 1, false},
	} {
		if validHostStart(tc.start, tc.pid) != tc.valid {
			t.Errorf("%s: validHostStart(%q, %d) != %v", tc.name, tc.start, tc.pid, tc.valid)
		}
	}
	useHost(t, liveHost{pid: 4242, uid: 501, start: fixtureStart})
	if got := hostStart(4242); got != fixtureStart {
		t.Fatalf("the host start was not captured: %q", got)
	}
	useHost(t, liveHost{readFails: true})
	if got := hostStart(4242); got != "" {
		t.Fatalf("an unreadable host got a start: %q", got)
	}
	useHost(t, liveHost{pid: 4242, uid: 501, start: "Tue Nov 14 08:02"})
	if got := hostStart(4242); got != "" {
		t.Fatalf("raw process text was saved as a start: %q", got)
	}
	now := time.Now()
	for name, end := range map[string]func(*pluginRunState){
		"failure":   func(r *pluginRunState) { r.recordFailure(now, "exited") },
		"permanent": func(r *pluginRunState) { r.recordPermanent(now, "pin_changed") },
		"stopped":   func(r *pluginRunState) { r.recordStopped(now, "disabled") },
	} {
		record := runningRecord()
		end(&record)
		if record.HostStart != "" || record.PID != 0 {
			t.Errorf("%s: the end kept host_start %q", name, record.HostStart)
		}
	}
	// An older running file without host_start loads in the controller as it is.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	old := runningRecord()
	old.HostStart = ""
	if err := saveRunState(dir, "alpha", old); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(runStatePath(dir, "alpha"))
	loaded, err := loadRunState(dir, "alpha", old.Installation)
	after, _ := os.ReadFile(runStatePath(dir, "alpha"))
	if err != nil || loaded.PID != old.PID || string(before) != string(after) || strings.Contains(string(after), "host_start") {
		t.Fatalf("an older running file did not load unchanged: %v", err)
	}
}

// The controller saves host_start for the host it starts (its own atomic
// save under its lock), and status then shows observed running with the
// host pid only (never plugin_pid). Real binary, real supervisor.
func TestPluginRuntimeObservedRunningCLI(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	state := supervisorFixture(t, binary, "alpha")
	startSupervisor(t, binary, state)
	run := awaitRunState(t, state, "alpha", running)
	if run.HostStart == "" {
		t.Fatalf("the controller's running save has no host_start for host pid %d", run.PID)
	}
	if info, err := procwatch.Read(run.PID); err != nil || info.Start != run.HostStart || !validHostStart(run.HostStart, run.PID) {
		t.Fatalf("the saved host_start does not match the host: %q %v", run.HostStart, err)
	}
	raw, err := pluginOwnerLocal(t, state, "status", "alpha")
	var status struct {
		Run map[string]json.RawMessage `json:"run"`
	}
	if err != nil || json.Unmarshal(raw, &status) != nil {
		t.Fatal(string(raw), err)
	}
	if string(status.Run["observed"]) != `"running"` || string(status.Run["pid"]) != strconv.Itoa(run.PID) || status.Run["plugin_pid"] != nil {
		t.Fatalf("status after the controller's save: %s", raw)
	}
}

// Status, list and doctor show only fixed codes and fields: never log
// content, child text, message bodies or capabilities; a stale or reused
// pid and the child pid are never printed; an invalid run file is kept
// byte-identical; the read-only commands create no file.
func TestPluginRuntimeStatusPrivacy(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	for _, args := range [][]string{{"add", "fake-notifier", "--exec", feedFixture(t)}, {"configure", "fake-notifier", "--scope", "all"}, {"grant", "fake-notifier", "--status"}, {"enable", "fake-notifier"}} {
		if raw, err := pluginOwnerLocal(t, store.Dir, args...); err != nil {
			t.Fatal(string(raw), err)
		}
	}
	const secret = "SECRET-LOG-LINE capability=c0ffee body=hello"
	if err := os.WriteFile(filepath.Join(store.Dir, "plugins", "fake-notifier.log"), []byte(secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	record := runningRecord()
	record.Installation = installationID(t, store.Dir, "fake-notifier")
	if err := saveRunState(store.Dir, "fake-notifier", record); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		host     liveHost
		observed string
	}{
		{"stale pid", liveHost{gone: true}, `"stopped"`},
		{"reused pid", liveHost{pid: 4242, uid: 501, start: "1700000999.000001"}, `"unknown"`},
		{"live host", liveHost{pid: 4242, uid: 501, start: fixtureStart}, `"running"`},
	} {
		useHost(t, tc.host)
		run := statusRun(t, store.Dir, "fake-notifier")
		if string(run["observed"]) != tc.observed || run["plugin_pid"] != nil || (tc.observed != `"running"` && run["pid"] != nil) || string(run["installed_by"]) != `"unknown"` {
			t.Fatalf("%s: status run %v", tc.name, run)
		}
	}
	checkReadOnlyPrivate(t, store.Dir, "fake-notifier")
	checkInvalidRunKept(t, store.Dir, "fake-notifier")
}

func statusRun(t *testing.T, state, name string) map[string]json.RawMessage {
	t.Helper()
	raw, err := pluginOwnerLocal(t, state, "status", name)
	var status struct {
		Run map[string]json.RawMessage `json:"run"`
	}
	if err != nil || json.Unmarshal(raw, &status) != nil {
		t.Fatal(string(raw), err)
	}
	return status.Run
}

func listPlugins(t *testing.T, state string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "plugins"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// One subtest per list phrase: each is printed only for its own state.
func TestPluginRuntimeListLines(t *testing.T) {
	useHost(t, liveHost{pid: 4242, uid: 501, start: fixtureStart})
	at := time.Date(2026, 11, 14, 8, 3, 12, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		record func(*pluginRunState)
		want   string
	}{
		{"running", nil, fmt.Sprintf(runPhraseRunning, at.Add(-70*time.Second).Local().Format("15:04"), 4242, 0)},
		{"starting", func(r *pluginRunState) { r.State, r.Since = runStarting, nil }, fmt.Sprintf(runPhraseStarting, 4242)},
		{"backoff", func(r *pluginRunState) {
			r.State, r.PID, r.HostStart, r.Since, r.NextRetryAt, r.ReasonCode, r.Restarts, r.BackoffStep = runBackoff, 0, "", nil, &at, "exited", []time.Time{time.Now()}, 1
		}, fmt.Sprintf(runPhraseBackoff, at.Local().Format("15:04:05"), "host exited", 1)},
		{"crashed", func(r *pluginRunState) {
			now := time.Now()
			r.State, r.PID, r.HostStart, r.Since, r.ReasonCode, r.LastReasonAt = runCrashed, 0, "", nil, "killed", &at
			r.Restarts = []time.Time{now, now, now, now, now}
		}, fmt.Sprintf(runPhraseCrashed, at.Local().Format("15:04"), 5, 5, "demo")},
		{"needs owner", func(r *pluginRunState) {
			r.State, r.PID, r.HostStart, r.Since, r.ReasonCode, r.LastReasonAt = runCrashed, 0, "", nil, "pin_changed", &at
		}, fmt.Sprintf(runPhraseReinstall, reasonText["pin_changed"])},
		{"prior run", func(r *pluginRunState) {
			r.State, r.PID, r.HostStart, r.Since, r.ReasonCode, r.LastReasonAt = runCrashed, 0, "", nil, "prior_run_unconfirmed", &at
		}, fmt.Sprintf(runPhraseNeedsYou, reasonText["prior_run_unconfirmed"], "demo")},
		{"lease held", func(r *pluginRunState) {
			r.State, r.PID, r.HostStart, r.Since, r.NextRetryAt, r.ReasonCode, r.BackoffStep = runBackoff, 0, "", nil, &at, "lease_held", 1
		}, fmt.Sprintf(runPhraseWaiting, at.Local().Format("15:04:05"), reasonText["lease_held"])},
		{"stopped", func(r *pluginRunState) { r.State, r.PID, r.HostStart, r.Since = runStopped, 0, "", nil }, runPhraseStopped},
		{"running but not proven", func(r *pluginRunState) { r.HostStart = "" }, runPhraseUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := runningRecord()
			since := at.Add(-70 * time.Second)
			record.Since = &since
			if tc.record != nil {
				tc.record(&record)
			}
			view := pluginRunView{Observed: observeRun("/state", record), Restarts10m: len(recentFailures(record.Restarts, time.Now()))}
			if got := runPhrase("demo", view, record, true); got != tc.want {
				t.Fatalf("%s line: got %q\nwant %q", tc.name, got, tc.want)
			}
		})
	}
	if got := runPhrase("demo", pluginRunView{}, pluginRunState{}, false); got != runPhraseUnknown {
		t.Fatalf("an unreadable record gave %q", got)
	}
}

// Doctor's plugins_background: the launcher only from a validated record,
// the missing-launcher notes only with positive absence, the disabled note,
// a custom state unsupported with no inspect, and inspect only (no change).
func TestPluginBackgroundDoctor(t *testing.T) {
	previous := backgroundPlatform
	backgroundPlatform = "darwin"
	t.Cleanup(func() { backgroundPlatform = previous })
	present := filepath.Join(t.TempDir(), "zalki")
	if err := os.WriteFile(present, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "gone", "zalki")
	target := &loginItemTarget{binary: present, home: "/h", state: "/h/s"}
	for _, tc := range []struct {
		name     string
		report   install.Report
		launcher string
		notes    []string
	}{
		{"owned, launcher present", reportOwned(present), present, nil},
		{"owned, launcher missing", reportOwned(missing), missing, []string{fmt.Sprintf(noteLauncherMissing, missing), noteBrewUninstall}},
		{"invalid record", install.Report{ManagedState: "invalid_record", ServiceState: "unknown"}, "", nil},
		{"disabled", reportDisabled, "", []string{loginItemDisabledLine}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fake := defaultHome(t, tc.report)
			view, launcher := backgroundView(t.Context(), target)
			notes := backgroundNotes(view, launcher)
			if launcher != tc.launcher || !slices.Equal(notes, tc.notes) || len(fake.changes) != 0 || view.Platform != "darwin" {
				t.Fatalf("doctor background: %q %v %v %+v", launcher, notes, fake.changes, view)
			}
			raw, _ := json.Marshal(doctorBackgroundView{pluginBackgroundView: view, Launcher: launcher, Notes: notes})
			if strings.Contains(string(raw), shownRecord) || strings.Contains(string(raw), "login_item_record") {
				t.Fatalf("the private record token was shown: %s", raw)
			}
		})
	}
	checkBackgroundNotRead(t, target)
}

// ZAL-64: a status or restart probe holds the controller lock shared for a
// moment. A controller starting in that moment must wait it out and take
// its lock, not exit as "already held"; a lock held for the whole bounded
// wait is still reported as held, within the bound.
func TestPluginSupervisorLockSurvivesStatusProbe(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(filepath.Join(state, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	first, _, err := acquireSupervisorLock(state) // creates the lock file
	if err != nil || first == nil {
		t.Fatal(err)
	}
	_ = first.Close()
	probe, err := os.OpenFile(filepath.Join(state, "plugins", ".supervisor.lock"), os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = probe.Close()
		close(released)
	}()
	controller, held, err := acquireSupervisorLock(state)
	<-released
	if err != nil || held || controller == nil {
		t.Fatalf("a short status probe made the controller give up its start: held %v err %v", held, err)
	}
	if !supervisorRunning(state) {
		t.Fatal("the probe did not see the controller")
	}
	start := time.Now()
	second, held, err := acquireSupervisorLock(state)
	if second != nil || !held || err != nil || time.Since(start) > 3*time.Second {
		t.Fatalf("a held lock was not reported as held within the bound: %v %v %s", held, err, time.Since(start))
	}
	_ = controller.Close()
	if supervisorRunning(state) {
		t.Fatal("the probe saw a controller after it stopped")
	}
}

// checkReadOnlyPrivate: status and list print no log or body text and
// create or remove no file.
func checkReadOnlyPrivate(t *testing.T, state, name string) {
	t.Helper()
	before := listPlugins(t, state)
	for _, args := range [][]string{{"status", name}, {"list"}} {
		raw, _ := pluginOwnerLocal(t, state, args...)
		if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "c0ffee") || strings.Contains(string(raw), "hello") {
			t.Fatalf("%v printed private text: %s", args, raw)
		}
	}
	if after := listPlugins(t, state); !slices.Equal(before, after) {
		t.Fatalf("a read-only command created or removed a file: %v -> %v", before, after)
	}
}

// checkInvalidRunKept: an invalid run file is kept byte-identical and shown
// as unknown.
func checkInvalidRunKept(t *testing.T, state, name string) {
	t.Helper()
	// An invalid run file: kept byte-identical, shown as unknown.
	path := runStatePath(state, name)
	if err := os.WriteFile(path, []byte(`{"schema":1,"state":"running"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := statusRun(t, state, name)
	if raw, _ := os.ReadFile(path); string(raw) != `{"schema":1,"state":"running"}` || string(run["observed"]) != `"unknown"` || string(run["reason_code"]) != `"run_state_invalid"` {
		t.Fatalf("an invalid run file was changed or trusted: %v", run)
	}
}

// checkBackgroundNotRead: a custom state or another platform never reads
// the login item.
func checkBackgroundNotRead(t *testing.T, target *loginItemTarget) {
	t.Helper()
	_, fake := defaultHome(t, reportNone)
	if view, launcher := backgroundView(t.Context(), nil); view.State != "unsupported" || view.Reason != "custom_state" || launcher != "" || len(fake.requests) != 0 {
		t.Fatalf("a custom state reached the login item: %+v %d", view, len(fake.requests))
	}
	backgroundPlatform = "linux"
	if view, _ := backgroundView(t.Context(), target); view.State != "unsupported" || view.Reason != "unsupported_platform" || len(fake.requests) != 0 {
		t.Fatalf("another platform read the login item: %+v", view)
	}
}
