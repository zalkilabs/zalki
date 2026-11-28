package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestOldSessionGuidanceKeepsHealthyRows(t *testing.T) {
	for _, kind := range []string{"owner", "missing", "null"} {
		t.Run(kind, func(t *testing.T) { checkOldSessionGuidance(t, kind) })
	}
}

func checkOldSessionGuidance(t *testing.T, kind string) {
	t.Helper()
	home := configTestHome(t)
	for _, key := range []string{"CODEX_HOME", "CURSOR_CONFIG_DIR", "CLAUDE_CONFIG_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	store := bus.StoreAt(filepath.Join(home, "state"), bus.WithRegistry(adapters.Builtin()))
	healthy, _, err := store.Register("codex", "00000000-0000-4000-8000-000000000071", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	old, _, err := store.Register("codex", "00000000-0000-4000-8000-000000000072", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	changeOldGuidanceRecord(&old, kind)
	path := filepath.Join(store.Dir(), "sessions", strings.ReplaceAll(old.ID, ":", "_")+".json")
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	output, warning, err := execute(t, home, "", "list", "--json", "--state", store.Dir())
	if err != nil || !strings.Contains(output, healthy.ID) || strings.Contains(output, old.ID) || warning != "[note] 1 session record cannot be read, so it is not listed.\nNext: run zalki doctor to see why.\n" {
		t.Fatal("OLD_SESSION_GUIDANCE_NOT_ONE_LINE_ONE_COMMAND", output, warning, err)
	}
	output, _, err = execute(t, home, "", "doctor", "--state", store.Dir())
	checkDoctorSessionReason(t, old.ID, output, err)
	checkOldGuidancePrivacy(t, store, home)
	if after, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, after) {
		t.Fatal("OLD_SESSION_GUIDANCE_CHANGED_RECORD", err)
	}
}

func changeOldGuidanceRecord(v *model.Session, kind string) {
	switch kind {
	case "owner":
		v.Owner = &model.ProcessRef{PID: 42, Start: "fixture", Role: "backend"}
	case "missing":
		v.PluginPolicy = nil
	case "null":
		v.PluginPolicy = json.RawMessage(`null`)
	}
}

func checkOldGuidancePrivacy(t *testing.T, store bus.Store, home string) {
	t.Helper()
	name := "private-name-secret\x1b.json"
	path := filepath.Join(store.Dir(), "sessions", name)
	if err := os.WriteFile(path, []byte("{private-json-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	output, _, err := execute(t, home, "", "doctor", "--state", store.Dir())
	if err != nil || strings.Contains(output, "private-name-secret") || strings.Contains(output, "private-json-secret") || !strings.Contains(output, "Skipped 1 session records with noncanonical filenames.") {
		t.Fatal("DOCTOR_EXPOSED_UNVALIDATED_SESSION_NAME", output, err)
	}
}

func checkDoctorSessionReason(t *testing.T, id, output string, err error) {
	t.Helper()
	if err != nil || !strings.Contains(output, "Skipped session "+id+": unsupported record.") || strings.Contains(output, "not migrated") || strings.Contains(output, "run zalki doctor") {
		t.Fatal("DOCTOR_LOST_FIXED_SESSION_REASON", output, err)
	}
}
