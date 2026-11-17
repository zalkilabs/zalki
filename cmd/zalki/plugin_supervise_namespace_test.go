package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// These owned-directory replacements characterize path publication after
// admission, not protection against a hostile replacement during a write.
func TestPluginRuntimePublicationUsesCurrentParentPath(t *testing.T) {
	t.Run("run state after supervisor admission", characterizeRunParentReplacement)
	t.Run("restart after request admission", characterizeRestartParentReplacement)
}

func characterizeRunParentReplacement(t *testing.T) {
	state, original := admittedPublicationParent(t)
	lock, held, err := acquireSupervisorLock(state)
	if err != nil || held {
		t.Fatalf("owned supervisor admission failed: held=%t err=%v", held, err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	oldParent := replacePublicationParent(t, state, original)
	run := pluginRunState{Schema: 1, Installation: model.NewUUID(), State: runStopped}
	want, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveRunState(state, "demo", run); err != nil {
		t.Fatal("run-state path publication refused the private replacement", err)
	}
	assertPublicationParent(t, runStatePath(state, "demo"), oldParent, want)
}

func characterizeRestartParentReplacement(t *testing.T) {
	state, original := admittedPublicationParent(t)
	previous := writeRestartRequest
	t.Cleanup(func() { writeRestartRequest = previous })
	var oldParent string
	var want []byte
	calls := 0
	// requestRestart reaches this existing seam only after taking its lock,
	// loading the old records and constructing the next request. The actual
	// default writer still performs the publication after the parent swap.
	writeRestartRequest = func(path string, raw []byte) error {
		calls++
		if path != restartPath(state, "demo") {
			t.Fatal("restart publication used an unexpected path", path)
		}
		oldParent = replacePublicationParent(t, state, original)
		want = bytes.Clone(raw)
		return journal.WriteAtomic(path, raw)
	}
	status := pluginfeed.PluginFeedStatus{ID: model.NewUUID()}
	if err := requestRestart(state, "demo", status, stateEpoch); err != nil {
		t.Fatal("restart path publication refused the private replacement", err)
	}
	if calls != 1 {
		t.Fatalf("restart publication calls = %d, want 1", calls)
	}
	assertPublicationParent(t, restartPath(state, "demo"), oldParent, want)
	request, ok, err := loadRestartRequest(state, "demo")
	if err != nil || !ok || request.Installation != status.ID || request.Seq != 1 || !request.RequestedAt.Equal(stateEpoch) {
		t.Fatalf("replacement directory did not receive the actual restart request: %+v %t %v", request, ok, err)
	}
}

func admittedPublicationParent(t *testing.T) (string, *os.File) {
	t.Helper()
	state := t.TempDir()
	parent := filepath.Join(state, "plugins")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := privateDirectory(parent); err != nil {
		t.Fatal("owned parent admission failed", err)
	}
	dir, err := os.Open(parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	return state, dir
}

func replacePublicationParent(t *testing.T, state string, original *os.File) string {
	t.Helper()
	parent, moved := filepath.Join(state, "plugins"), filepath.Join(state, "admitted-plugins")
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := privateDirectory(parent); err != nil {
		t.Fatal("owned replacement is not private", err)
	}
	retained, err := original.Stat()
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(moved)
	if err != nil || !os.SameFile(retained, old) {
		t.Fatal("the admitted directory handle was not retained", err)
	}
	current, err := os.Stat(parent)
	if err != nil || os.SameFile(retained, current) {
		t.Fatal("the owned parent was not replaced", err)
	}
	return moved
}

func assertPublicationParent(t *testing.T, path, oldParent string, want []byte) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, want) {
		t.Fatalf("current parent did not receive the exact publication: %q %v", raw, err)
	}
	info, err := os.Stat(path)
	if err != nil || !privateFile(info) || info.Mode().Perm() != 0o600 {
		t.Fatal("publication permissions changed", err)
	}
	if _, err := os.Lstat(filepath.Join(oldParent, filepath.Base(path))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("publication unexpectedly reached the admitted directory", err)
	}
}
