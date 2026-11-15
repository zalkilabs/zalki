package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestMixedRuntimeHeadersKeepDoctorAndPruneAvailable(t *testing.T) {
	t.Run("mixed", checkMixedRuntimeHeaders)
	for _, kind := range []string{"malformed", "unreadable", "identity", "directory"} {
		t.Run(kind, func(t *testing.T) { checkRuntimeScanError(t, kind) })
	}
}

func checkMixedRuntimeHeaders(t *testing.T) {
	t.Helper()
	s := fixtureStore(t)
	t.Setenv("PATH", t.TempDir())
	healthy, _ := register(t, s, "codex")
	lease, err := s.TrackRuntime("mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	stale := lease.Info()
	stale.ID = newID()
	stale.Process = &model.ProcessRef{PID: os.Getpid(), Start: "exited-runtime-instance", Role: "zalki"}
	if err := s.locked(func() error { return writeJSON(filepath.Join(s.Dir, "runtimes", stale.ID+".json"), stale) }); err != nil {
		t.Fatal(err)
	}
	old := stale
	old.ID, old.Role = newID(), "unsupported-runtime-secret"
	path := filepath.Join(s.Dir, "runtimes", old.ID+".json")
	raw, err := json.Marshal(old) // Deliberately no private runtimeRecord header.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
	checkMixedRuntimeDoctor(t, s, healthy.ID, lease.Info().ID, old.ID)
	checkMixedRuntimePrune(t, s, stale.ID, path, raw, at)
}

func checkMixedRuntimeDoctor(t *testing.T, s Store, session, live, old string) {
	t.Helper()
	report, err := s.Diagnostics(t.Context())
	if err != nil || len(report.Sessions) != 1 || report.Sessions[0].ID != session {
		t.Fatal("MIXED_RUNTIME_DOCTOR_DID_NOT_CONTINUE", err)
	}
	if len(report.Runtimes) != 1 || report.Runtimes[0].ID != live {
		t.Fatal("MIXED_RUNTIME_LIVENESS_USED_UNSUPPORTED", report.Runtimes)
	}
	notes := strings.Join(report.Notes, "\n")
	if strings.Count(notes, "Skipped 1 unsupported runtime records;") != 1 || strings.Contains(notes, old) || strings.Contains(notes, "unsupported-runtime-secret") {
		t.Fatal("MIXED_RUNTIME_DOCTOR_SKIP_NOTE", notes)
	}
}

func checkMixedRuntimePrune(t *testing.T, s Store, exited, path string, raw []byte, at time.Time) {
	t.Helper()
	for _, dry := range []bool{true, false} {
		report, err := s.Prune(dry, nil)
		if err != nil || len(report.Runtimes) != 1 || report.Runtimes[0] != exited {
			t.Fatal("MIXED_RUNTIME_PRUNE_DID_NOT_CONTINUE", dry, report.Runtimes, err)
		}
		checkSkippedRuntimeUnchanged(t, path, raw, at)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, "runtimes", exited+".json")); !os.IsNotExist(err) {
		t.Fatal("MIXED_RUNTIME_VALID_EXIT_WAS_NOT_DELETED", err)
	}
}

func checkSkippedRuntimeUnchanged(t *testing.T, path string, raw []byte, at time.Time) {
	t.Helper()
	after, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || !bytes.Equal(raw, after) || !info.ModTime().Equal(at) {
		t.Fatal("UNSUPPORTED_RUNTIME_WAS_CHANGED", err, statErr)
	}
}

func checkRuntimeScanError(t *testing.T, kind string) {
	t.Helper()
	s := fixtureStore(t)
	lease, err := s.TrackRuntime("mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(s.Dir, "runtimes")
	path := filepath.Join(root, newID()+".json")
	switch kind {
	case "directory":
		if err := os.Remove(root); err != nil {
			t.Fatal(err)
		}
		path = root
		if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
	case "unreadable":
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	case "identity":
		if err := writeJSON(path, lease.Info()); err != nil {
			t.Fatal(err)
		}
	default:
		if err := os.WriteFile(path, []byte("{malformed"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.rawRuntimes(); err == nil {
		t.Fatal("RUNTIME_SCAN_REAL_ERROR_WAS_SKIPPED", kind)
	}
}
