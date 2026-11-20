package bus

import (
	"bytes"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type profileFixtureAdapter struct {
	fixtureAdapter
	root     *string
	captured func()
}

func (a profileFixtureAdapter) ConfigDir() string {
	root := *a.root
	if a.captured != nil {
		a.captured()
	}
	return root
}
func profileFixtureStore(t *testing.T, root *string, captured func()) Store {
	t.Helper()
	registry, err := agent.NewRegistry(profileFixtureAdapter{fixtureAdapter: fixtureAdapter{kind: "fourth", inbox: model.InboxNative}, root: root, captured: captured})
	if err != nil {
		t.Fatal(err)
	}
	s := fixtureStore(t)
	s.registry = registry
	return s
}
func TestRegistrationRequiresAbsoluteNativeProfile(t *testing.T) {
	for _, suffix := range []string{"soft\u00adhyphen", "zero\u200djoiner", " spaces "} {
		t.Run(suffix, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), suffix)
			s := profileFixtureStore(t, &root, nil)
			v, key, err := s.Register("fourth", newID(), "/workspace", nil)
			if err != nil || key == "" || v.NativeConfigDir != root {
				t.Fatal("CONTROL_FREE_ROOT_WAS_REFUSED", err)
			}
		})
	}
	for _, root := range []string{"", "relative/profile", "~/profile", "/bad\x00profile", "/bad\nprofile", "/bad\x7fprofile", "/bad\x85profile", "/bad\xffprofile"} {
		t.Run(root, func(t *testing.T) {
			s := profileFixtureStore(t, &root, nil)
			_, key, err := s.Register("fourth", newID(), "/workspace", nil)
			if model.ErrorCodeOf(err) != model.CodeInvalidInput || key != "" {
				t.Fatal("INVALID_ROOT_REGISTERED", key != "", err)
			}
			if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
				t.Fatal("INVALID_ROOT_CREATED_STATE", err)
			}
		})
	}
}
func TestStoredNativeProfileRefusalKeepsAuthorityAndMail(t *testing.T) {
	for _, bad := range []string{"", "relative/profile"} {
		t.Run(bad, func(t *testing.T) { checkStoredNativeProfileRefusal(t, bad) })
	}
}
func checkStoredNativeProfileRefusal(t *testing.T, bad string) {
	t.Helper()
	root := t.TempDir()
	s := profileFixtureStore(t, &root, nil)
	v, key := register(t, s, "fourth")
	sender, _ := register(t, s, "fourth")
	held := queuedFor(t, s, sender, v.ID)
	v.ApprovalHeld = 1
	v.NativeConfigDir = bad
	path, _ := s.sessionPath(v.ID)
	if err := writeJSON(path, v); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	before := readCommitFile(t, path)
	if _, err := s.authenticate(v.ID, key); model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatal("ROOTLESS_SESSION_AUTHENTICATED", err)
	}
	for _, compact := range []bool{false, true} {
		if _, replacement, err := s.register(v.Kind, v.NativeID, v.CWD, nil, compact); model.ErrorCodeOf(err) != model.CodeUnavailable || replacement != "" {
			t.Fatal("ROOTLESS_REGISTRATION_REPLACED", err)
		}
	}
	rows, skipped, err := s.ListWithWarnings()
	if err != nil || len(rows) != 1 || rows[0].ID != sender.ID || skipped != 1 {
		t.Fatal("ROOTLESS_LIST_LOST_HEALTHY_ROW", rows, skipped, err)
	}
	report, err := s.Diagnostics(t.Context())
	if err != nil || !strings.Contains(strings.Join(report.Notes, "\n"), "Skipped session "+v.ID+": missing absolute config root.") {
		t.Fatal("DOCTOR_LOST_FIXED_ROOT_REASON", err, report.Notes)
	}
	checkBackfillPruneKeepsMail(t, s, v.ID, held)
	checkBackfillRecordUnchanged(t, path, before, at)
}
func TestCompactionCannotChangeNativeProfile(t *testing.T) {
	root := t.TempDir()
	s := profileFixtureStore(t, &root, nil)
	v, key := register(t, s, "fourth")
	sender, _ := register(t, s, "fourth")
	held := queuedFor(t, s, sender, v.ID)
	v = withHeld(t, s, v, 1)
	path, _ := s.sessionPath(v.ID)
	before := readCommitFile(t, path)
	root = t.TempDir()
	_, replacement, err := s.register(v.Kind, v.NativeID, v.CWD, nil, true)
	if model.ErrorCodeOf(err) != model.CodeConflict || replacement != "" {
		t.Fatal("COMPACT_PROFILE_CHANGED", err, replacement != "")
	}
	if !strings.Contains(err.Error(), "start a fresh session") || strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "doctor") {
		t.Fatal("COMPACT_PROFILE_GUIDANCE", err)
	}
	if !bytes.Equal(before, readCommitFile(t, path)) {
		t.Fatal("COMPACT_PROFILE_CHANGED_RECORD")
	}
	if _, err := s.authenticate(v.ID, key); err != nil {
		t.Fatal("COMPACT_PROFILE_INVALIDATED_OLD_KEY", err)
	}
	if current := journalMessage(t, s, held.ID); !current.Held {
		t.Fatal("COMPACT_PROFILE_SETTLED_MAIL")
	}
	root = v.NativeConfigDir
	next, _, err := s.register(v.Kind, v.NativeID, v.CWD, nil, true)
	if err != nil || next.Generation != v.Generation || next.ApprovalHeld != 1 {
		t.Fatal("SAME_PROFILE_COMPACT_CHANGED_IDENTITY", next, err)
	}
	root = t.TempDir()
	next, _, err = s.Register(v.Kind, v.NativeID, v.CWD, nil)
	if err != nil || next.Generation == v.Generation || next.NativeConfigDir != root {
		t.Fatal("NEW_REGISTRATION_COULD_NOT_CHANGE_PROFILE", next, err)
	}
}
func TestCompactionComparesProfileAfterTakingJournalLock(t *testing.T) {
	root := t.TempDir()
	ready := make(chan struct{})
	var captured func()
	s := profileFixtureStore(t, &root, func() {
		if captured != nil {
			captured()
		}
	})
	v, _ := register(t, s, "fourth")
	captured = func() { close(ready) }
	type result struct {
		key string
		err error
	}
	done := make(chan result, 1)
	newRoot := filepath.Join(t.TempDir(), "new")
	err := s.locked(func() error {
		go func() { _, key, err := s.register(v.Kind, v.NativeID, v.CWD, nil, true); done <- result{key, err} }()
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("profile capture did not reach journal waiter")
		}
		v.NativeConfigDir = newRoot
		return s.saveSession(v)
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if model.ErrorCodeOf(got.err) != model.CodeConflict || got.key != "" {
			t.Fatal("COMPACT_PROFILE_USED_PRELOCK_IMAGE", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("compact did not join after journal release")
	}
	current, err := s.session(v.ID)
	if err != nil || current.NativeConfigDir != newRoot {
		t.Fatal("COMPACT_PROFILE_OVERWROTE_LOCKED_IMAGE", current, err)
	}
}
