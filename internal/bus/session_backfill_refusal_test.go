package bus

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestObsoleteSessionBackfillsAreRefused(t *testing.T) {
	for _, test := range []struct {
		name, marker string
		change       func(*model.Session)
	}{
		{"owner", "OLD_OWNER_SESSION_ACCEPTED", func(v *model.Session) { v.Owner = &model.ProcessRef{PID: 42, Start: "fixture", Role: "client"} }},
		{"missing_policy", "MISSING_POLICY_SESSION_ACCEPTED", func(v *model.Session) { v.PluginPolicy = nil }},
		{"null_policy", "MISSING_POLICY_SESSION_ACCEPTED", func(v *model.Session) { v.PluginPolicy = json.RawMessage(`null`) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := ownerStore(t, sharedAdapter{})
			v, key := register(t, s, "cursor")
			sender, _ := register(t, s, "codex")
			queued := queuedFor(t, s, sender, v.ID)
			v.ApprovalHeld = 1
			test.change(&v)
			path, _ := s.sessionPath(v.ID)
			if err := writeJSON(path, v); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
			if err := os.Chtimes(path, at, at); err != nil {
				t.Fatal(err)
			}
			before := readCommitFile(t, path)
			if _, err := s.session(v.ID); model.ErrorCodeOf(err) != model.CodeUnavailable {
				t.Fatal(test.marker, err)
			}
			checkBackfillAuthorityRefusals(t, s, v, key)
			checkBackfillPruneKeepsMail(t, s, v.ID, queued)
			checkBackfillRecordUnchanged(t, path, before, at)
		})
	}
}

func checkBackfillAuthorityRefusals(t *testing.T, s Store, v model.Session, key string) {
	t.Helper()
	for name, run := range map[string]func() error{
		"authenticate": func() error { _, err := s.authenticate(v.ID, key); return err },
		"register":     func() error { _, _, err := s.Register(v.Kind, v.NativeID, v.CWD, nil); return err },
		"compact":      func() error { _, _, err := s.register(v.Kind, v.NativeID, v.CWD, nil, true); return err },
		"refresh":      func() error { return s.rememberOwner(v.ID, v.Kind, true) },
	} {
		if err := run(); model.ErrorCodeOf(err) != model.CodeUnavailable {
			t.Fatal("OLD_SESSION_AUTHORITY_WAS_USED", name, err)
		}
	}
	input, err := json.Marshal(map[string]string{"conversation_id": v.NativeID, "hook_event_name": "beforeSubmitPrompt", "cwd": v.CWD})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := (&Service{Store: s}).Hook(t.Context(), "cursor", bytes.NewReader(input), &output); err != nil || !strings.Contains(output.String(), `"continue":true`) {
		t.Fatal("OLD_SESSION_BLOCKED_OWNER_PROMPT", output.String(), err)
	}
	rows, skipped, err := s.ListWithWarnings()
	if err != nil || len(rows) != 1 || skipped != 1 {
		t.Fatal("OLD_SESSION_LIST_LOST_HEALTHY_ROW_OR_WARNING", len(rows), skipped, err)
	}
}

func checkBackfillRecordUnchanged(t *testing.T, path string, before []byte, at time.Time) {
	t.Helper()
	after, err := os.ReadFile(path)
	info, statErr := os.Stat(path)
	if err != nil || statErr != nil || !bytes.Equal(before, after) || !info.ModTime().Equal(at) {
		t.Fatal("OLD_SESSION_REFUSAL_CHANGED_RECORD", err, statErr)
	}
	var saved model.Session
	if err := json.Unmarshal(after, &saved); err != nil || saved.ApprovalHeld != 1 {
		t.Fatal("OLD_SESSION_REFUSAL_CHANGED_APPROVAL_COUNT", err, saved.ApprovalHeld)
	}
}

func checkBackfillPruneKeepsMail(t *testing.T, s Store, id string, queued model.Message) {
	t.Helper()
	for _, dry := range []bool{true, false} {
		if _, err := s.Prune(dry, []string{id}); err != nil {
			t.Fatal(err)
		}
		if current := journalMessage(t, s, queued.ID); !current.Held || current.Status != queued.Status {
			t.Fatal("UNUSABLE_TARGET_SETTLED_HELD_MAIL")
		}
	}
}
