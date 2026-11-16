package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func ownerRun(t *testing.T, state string, args ...string) {
	t.Helper()
	if raw, err := pluginOwnerLocal(t, state, args...); err != nil {
		t.Fatal(args, string(raw), err)
	}
}

func restartPending(t *testing.T, state string) (restartRequest, bool) {
	t.Helper()
	request, ok, err := loadRestartRequest(state, "demo")
	if err != nil {
		t.Fatal(err)
	}
	return request, ok
}

// S13 remainder: a confirmed disabled->enabled change writes the restart
// request that clears the crash limit; an enable that was already true and
// a grant on an enabled plugin write none; a failed request after a good
// enable is a one-line partial success with exit 1.
func TestPluginEnableResetsCrashLimit(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	state := store.Dir
	ownerRun(t, state, "add", "demo", "--exec", feedFixture(t))
	// An enable that fails writes no request.
	if _, err := pluginOwnerLocal(t, state, "enable", "demo"); err == nil {
		t.Fatal("an unconfigured plugin was enabled")
	}
	if _, ok := restartPending(t, state); ok {
		t.Fatal("a restart request was written for an enable that failed")
	}
	ownerRun(t, state, "configure", "demo", "--scope", "all")
	ownerRun(t, state, "grant", "demo", "--status")
	id := installationID(t, state, "demo")
	crashed := pluginRunState{Schema: 1, Installation: id, State: runStopped}
	for i := range crashLimit {
		crashed.recordFailure(time.Now().Add(time.Duration(i-crashLimit)*time.Second), "failed")
	}
	if err := saveRunState(state, "demo", crashed); err != nil {
		t.Fatal(err)
	}
	if raw, err := pluginOwnerLocal(t, state, "enable", "demo"); err != nil || strings.TrimSpace(string(raw)) != "Enabled demo." {
		t.Fatalf("enable: %q %v", raw, err)
	}
	request, ok := restartPending(t, state)
	if !ok || request.Installation != id || request.Seq != 1 {
		t.Fatalf("a disabled->enabled change wrote no restart request: %+v", request)
	}
	checkResetConsumed(t, state, id)
	ownerRun(t, state, "enable", "demo")
	if _, ok := restartPending(t, state); ok {
		t.Fatal("an already-enabled plugin wrote a restart request")
	}
	before := pluginEpoch(t, state)
	ownerRun(t, state, "grant", "demo", "--receive")
	if pluginEpoch(t, state) == before {
		t.Fatal("the grant did not change the binding")
	}
	if _, ok := restartPending(t, state); ok {
		t.Fatal("a grant on an enabled plugin wrote a restart request")
	}
	checkEnablePartial(t, state)
}

// checkResetConsumed: the controller applies the request and clears the limit.
func checkResetConsumed(t *testing.T, state, id string) {
	t.Helper()
	status, err := pluginfeed.NewPluginFeed(bus.Store{Dir: state}, "demo").PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	s := &supervisor{state: state, saveRun: saveRunState, instance: model.NewUUID(), now: time.Now, plugins: map[string]*supervised{}}
	p := s.supervisedFor("demo", nil, id)
	if p == nil || !s.consumeRestart("demo", p, bindingKey{status.ID, status.Epoch, status.Revision}) {
		t.Fatal("the controller did not apply the enable's restart request")
	}
	if run, err := loadRunState(state, "demo", id); err != nil || run.State != runStopped || len(run.Restarts) != 0 || run.RestartSeq != 1 {
		t.Fatalf("the crash limit was not cleared: %+v %v", run, err)
	}
}

// checkEnablePartial: the request cannot be written (its path is a folder).
func checkEnablePartial(t *testing.T, state string) {
	t.Helper()
	ownerRun(t, state, "disable", "demo")
	if err := os.Mkdir(restartPath(state, "demo"), 0o700); err != nil {
		t.Fatal(err)
	}
	raw, err := pluginOwnerLocal(t, state, "enable", "demo")
	if err == nil {
		t.Fatalf("a failed request write was reported as success: %q", raw)
	}
	if err.Error() != "Enabled demo. Clearing its crash limit could not be confirmed. Run: zalki plugins restart demo" {
		t.Fatalf("the partial enable line is not the reviewed text: %v", err)
	}
	status, statusErr := pluginfeed.NewPluginFeed(bus.Store{Dir: state}, "demo").PluginStatus()
	if statusErr != nil || !status.Enabled {
		t.Fatal("the partial enable did not keep the plugin enabled")
	}
	_ = os.Remove(filepath.Clean(restartPath(state, "demo")))
	checkEnableLatePublication(t, state)
}

// checkEnableLatePublication: the request becomes visible and then the write
// reports an error (a late directory sync). The line must not claim that
// nothing was cleared: clearing is only "not confirmed".
func checkEnableLatePublication(t *testing.T, state string) {
	t.Helper()
	ownerRun(t, state, "disable", "demo")
	old := writeRestartRequest
	writeRestartRequest = func(path string, raw []byte) error {
		if err := old(path, raw); err != nil {
			return err
		}
		return errors.New("late directory sync failure")
	}
	t.Cleanup(func() { writeRestartRequest = old })
	_, err := pluginOwnerLocal(t, state, "enable", "demo")
	writeRestartRequest = old
	if _, published := restartPending(t, state); !published {
		t.Fatal("the late-publication fixture did not publish the request")
	}
	if err == nil || err.Error() != "Enabled demo. Clearing its crash limit could not be confirmed. Run: zalki plugins restart demo" {
		t.Fatalf("a published request with a late error was reported wrongly: %v", err)
	}
}

func pluginEpoch(t *testing.T, state string) string {
	t.Helper()
	status, err := pluginfeed.NewPluginFeed(bus.Store{Dir: state}, "demo").PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	return status.Epoch + "/" + status.Revision
}
