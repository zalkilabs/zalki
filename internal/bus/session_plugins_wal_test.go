package bus

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionPluginWALReplayKeepsPreparedAudit(t *testing.T) {
	t.Setenv("ZALKI_PLUGINS", "external,notifier")
	for _, kind := range []string{"constructor", "retired", "owner"} {
		for cut := range 4 {
			t.Run(kind+string(rune('0'+cut)), func(t *testing.T) { assertSessionPluginWALCut(t, kind, cut) })
		}
	}
}

func assertSessionPluginWALCut(t *testing.T, kind string, cut int) {
	t.Helper()
	s := fixtureStore(t)
	next, prior := sessionPluginWALImage(t, s, kind)
	event, err := sessionEvent(prior, next, model.CauseRegister)
	if err != nil {
		t.Fatal(err)
	}
	writer := commitWriter{dir: s.Dir}
	batch, err := writer.prepare(prior, next, event)
	if err != nil {
		t.Fatal(err)
	}
	var prepared model.Session
	if err := json.Unmarshal(batch.Puts[0].Data, &prepared); err != nil {
		t.Fatal(err)
	}
	want, valid := decodeSessionPluginPolicy(prepared)
	if !valid {
		t.Fatal("Z6_WAL_AFTER_IMAGE_HAS_COMPLETE_POLICY")
	}
	if err := ensureEventDirectories(s, orderEventDirectory, kind != "constructor"); err != nil {
		t.Fatal(err)
	}
	stageCommit(t, commitFixture{writer: writer}, batch)
	for _, put := range batch.Puts[:cut] {
		if err := journal.WriteAtomic(filepath.Join(s.Dir, put.Path), put.Data); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := s.locked(func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		got, policy := pluginPolicyForTest(t, s, next.ID)
		actual, _ := json.Marshal(policy)
		expected, _ := json.Marshal(want)
		if !bytes.Equal(actual, expected) || got.Retired != next.Retired || !samePluginOwner(got.Owner, next.Owner) {
			t.Fatal("Z6_WAL_REPLAY_COMPLETE_POLICY_AND_AUDIT_STABLE")
		}
	}
}

func sessionPluginWALImage(t *testing.T, s Store, kind string) (model.Session, []byte) {
	t.Helper()
	var next model.Session
	var prior []byte
	var err error
	if kind == "constructor" {
		next, _, err = newRegistration(s, "cursor", newID(), "/workspace", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.locked(func() error { return nil }); err != nil {
			t.Fatal(err)
		}
	} else {
		var key string
		next, key = register(t, s, "cursor")
		if err := SetSessionPlugin(s, next.ID, key, "external", true); err != nil {
			t.Fatal(err)
		}
		next, err = s.session(next.ID)
		if err != nil {
			t.Fatal(err)
		}
		path, _ := s.sessionPath(next.ID)
		prior = readCommitFile(t, path)
		if kind == "retired" {
			next.State = model.StateRetired
			next.Retired = true
		} else {
			next.State = model.StateBusy
			next.Owner = &model.ProcessRef{PID: 800001, Start: "new-owner", Role: "client"}
		}
		if err := refreshSessionPluginAudit(prior, &next); err != nil {
			t.Fatal(err)
		}
	}
	return next, prior
}
