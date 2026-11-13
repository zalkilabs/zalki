package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func restartFixture(t *testing.T) (*supervisor, *supervised, bindingKey) {
	t.Helper()
	state := t.TempDir()
	if err := os.Mkdir(filepath.Join(state, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	logs, err := openRotatingLog(filepath.Join(state, "plugins", ".supervisor.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logs.close() })
	key := bindingKey{model.NewUUID(), "epoch-1", "revision-1"}
	run := pluginRunState{Schema: 1, Installation: key.id, State: runStopped}
	for i := range crashLimit {
		run.recordFailure(stateEpoch.Add(time.Duration(i)*time.Minute), "failed")
	}
	s := &supervisor{state: state, log: logs, saveRun: saveRunState, instance: model.NewUUID(), now: func() time.Time { return stateEpoch.Add(time.Hour) }}
	return s, &supervised{run: run}, key
}

func writeRequest(t *testing.T, state string, request restartRequest) {
	t.Helper()
	raw, err := json.Marshal(request)
	if err == nil {
		err = journal.WriteAtomic(restartPath(state, "demo"), raw)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func requestFor(key bindingKey, seq uint64) restartRequest {
	return restartRequest{Installation: key.id, Epoch: key.epoch, Revision: key.revision, Seq: seq, Nonce: model.NewUUID(), RequestedAt: stateEpoch}
}

// S13: a matching request clears the crash limit and records its sequence
// in the same durable write; the request is then removed.
func TestPluginRestartRequestResets(t *testing.T) {
	s, p, key := restartFixture(t)
	if err := requestRestart(s.state, "demo", pluginfeed.PluginFeedStatus{ID: key.id, Epoch: key.epoch, Revision: key.revision}, stateEpoch); err != nil {
		t.Fatal(err)
	}
	if !s.consumeRestart("demo", p, key) || p.run.State != runStopped || len(p.run.Restarts) != 0 || p.run.BackoffStep != 0 || p.run.RestartSeq != 1 {
		t.Fatalf("a matching restart did not reset: %+v", p.run)
	}
	saved, err := loadRunState(s.state, "demo", key.id)
	if err != nil || saved.RestartSeq != 1 || len(saved.Restarts) != 0 {
		t.Fatalf("the reset and its sequence were not saved together: %+v %v", saved, err)
	}
	if _, err := os.Lstat(restartPath(s.state, "demo")); !os.IsNotExist(err) {
		t.Fatal("the consumed request stayed", err)
	}
}

// S13: after a crash between the reset write and the request's removal,
// later failures were recorded; the replayed request (equal or older
// sequence) is cleanup only and never clears them.
func TestPluginRestartRequestReplayIsCleanupOnly(t *testing.T) {
	for _, seq := range []uint64{3, 2} {
		s, p, key := restartFixture(t)
		p.run.RestartSeq = 3
		p.run.Restarts = p.run.Restarts[:2]
		p.run.State = runBackoff
		writeRequest(t, s.state, requestFor(key, seq))
		if s.consumeRestart("demo", p, key) || len(p.run.Restarts) != 2 || p.run.RestartSeq != 3 {
			t.Fatalf("seq %d: a consumed request reset newer failures: %+v", seq, p.run)
		}
		if _, err := os.Lstat(restartPath(s.state, "demo")); !os.IsNotExist(err) {
			t.Fatalf("seq %d: the replayed request was not cleaned up", seq)
		}
	}
}

// S13: a request for another installation or binding is never applied.
func TestPluginRestartRequestForAnotherBindingIsIgnored(t *testing.T) {
	for name, change := range map[string]func(*restartRequest){
		"installation": func(r *restartRequest) { r.Installation = model.NewUUID() },
		"epoch":        func(r *restartRequest) { r.Epoch = "epoch-0" },
		"revision":     func(r *restartRequest) { r.Revision = "revision-0" },
	} {
		s, p, key := restartFixture(t)
		request := requestFor(key, 1)
		change(&request)
		writeRequest(t, s.state, request)
		if s.consumeRestart("demo", p, key) || p.run.State != runCrashed || len(p.run.Restarts) != crashLimit {
			t.Fatalf("%s: a stale request was applied: %+v", name, p.run)
		}
	}
}

// S13: a new request is numbered above both the pending request and the
// consumed high-water mark.
func TestPluginRestartRequestSequence(t *testing.T) {
	s, p, key := restartFixture(t)
	p.run.RestartSeq = 5
	if err := saveRunState(s.state, "demo", p.run); err != nil {
		t.Fatal(err)
	}
	status := pluginfeed.PluginFeedStatus{ID: key.id, Epoch: key.epoch, Revision: key.revision}
	for _, want := range []uint64{6, 7} {
		if err := requestRestart(s.state, "demo", status, stateEpoch); err != nil {
			t.Fatal(err)
		}
		request, ok, err := loadRestartRequest(s.state, "demo")
		if err != nil || !ok || request.Seq != want {
			t.Fatalf("request numbered %d, want %d: %v", request.Seq, want, err)
		}
	}
}
