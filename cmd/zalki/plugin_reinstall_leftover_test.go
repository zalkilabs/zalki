package main

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// After remove and a same-NAME reinstall, the old installation's
// NAME.run.json and NAME.restart.json stay byte-identical and are ignored:
// its crash history and its restart request never apply to the new one.
// (Normal controller processing may later replace or clean them.)
func TestPluginReinstallIgnoresLeftoverRunFiles(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	old := installWithLeftovers(t, store)
	runBefore, requestBefore := readLeftover(t, store.Dir())
	for _, args := range [][]string{{"remove", "demo"}, {"add", "demo", "--exec", feedFixture(t)}} {
		if raw, err := pluginOwnerLocal(t, store.Dir(), args...); err != nil {
			t.Fatal(string(raw), err)
		}
	}
	key := bindingOf(t, store, "demo")
	if key.id == old.id {
		t.Fatal("the reinstall kept the old installation ID")
	}
	if runAfter, requestAfter := readLeftover(t, store.Dir()); !bytes.Equal(runAfter, runBefore) || !bytes.Equal(requestAfter, requestBefore) {
		t.Fatal("remove or reinstall changed the old installation's run files")
	}
	loaded, err := loadRunState(store.Dir(), "demo", key.id)
	if err != nil || loaded.Installation != key.id || len(loaded.Restarts) != 0 || loaded.State != runStopped || loaded.BackoffStep != 0 {
		t.Fatalf("the old installation's crash history was applied to the new one: %+v %v", loaded, err)
	}
	s, p, _ := restartFixture(t)
	s.state, p.run = store.Dir(), loaded
	if s.consumeRestart("demo", p, key) || p.run.RestartSeq != 0 {
		t.Fatalf("the old installation's restart request was applied to the new one: seq %d", p.run.RestartSeq)
	}
}

// installWithLeftovers installs "demo" and gives it a crashed history and
// a pending restart request.
func installWithLeftovers(t *testing.T, store bus.Store) bindingKey {
	t.Helper()
	if raw, err := pluginOwnerLocal(t, store.Dir(), "add", "demo", "--exec", feedFixture(t)); err != nil {
		t.Fatal(string(raw), err)
	}
	old := bindingOf(t, store, "demo")
	run := pluginRunState{Schema: 1, Installation: old.id, State: runStopped}
	for i := range crashLimit {
		run.recordFailure(stateEpoch.Add(time.Duration(i)*time.Minute), "failed")
	}
	if err := saveRunState(store.Dir(), "demo", run); err != nil {
		t.Fatal(err)
	}
	writeRequest(t, store.Dir(), requestFor(old, 1))
	return old
}

func bindingOf(t *testing.T, store bus.Store, name string) bindingKey {
	t.Helper()
	snap, err := pluginfeed.NewPluginFeed(store, name).PluginSnapshot()
	if err != nil || snap.ID == "" {
		t.Fatal("no installation", err)
	}
	return bindingKey{snap.ID, snap.Epoch, snap.Revision}
}

func readLeftover(t *testing.T, state string) (run, request []byte) {
	t.Helper()
	run, err := os.ReadFile(runStatePath(state, "demo"))
	if err == nil {
		request, err = os.ReadFile(restartPath(state, "demo"))
	}
	if err != nil {
		t.Fatal(err)
	}
	return run, request
}
