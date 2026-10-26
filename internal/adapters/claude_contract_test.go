package adapters

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

type claudeContract struct {
	Version       string          `json:"version"`
	VersionOutput string          `json:"version_output"`
	Hook          json.RawMessage `json:"hook"`
	NameRecord    json.RawMessage `json:"name_record"`
	Frame         map[string]any  `json:"frame"`
}

func loadClaudeContract(t *testing.T) claudeContract {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "bus", "testdata", "clients", "claude.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture claudeContract
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestRecordedClaudeHookAndMetadataContract(t *testing.T) {
	fixture := loadClaudeContract(t)
	adapter := claudeAdapter{}
	var hook bus.HookInput
	if err := json.Unmarshal(fixture.Hook, &hook); err != nil {
		t.Fatal(err)
	}
	hook, err := adapter.NormalizeHook(hook)
	if err != nil || hook.SessionID != "00000000-0000-4000-8000-000000000001" || hook.CWD != "/fixture/workspace" || adapter.Descriptor().Hooks[hook.Event] != "SessionStart" {
		t.Fatal("recorded native hook no longer normalizes", err)
	}
	if fixture.Version != "unknown" && clientVersionPattern.FindString(fixture.VersionOutput) != fixture.Version {
		t.Fatal("native version output no longer parses")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sessions", hook.SessionID+".json"), fixture.NameRecord, 0600); err != nil {
		t.Fatal(err)
	}
	name, source := adapter.LookupName(bus.Session{Kind: "claude", NativeID: hook.SessionID, NativeConfigDir: root}, map[string]map[string]string{})
	if name != "Contract review" || source != "claude_session_metadata" {
		t.Fatalf("native metadata contract changed: %q %q", name, source)
	}
}

func TestClaudeMetadataBoundsAndConflicts(t *testing.T) {
	root := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessions, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(file, id, name string) {
		t.Helper()
		if err := writeTestJSON(filepath.Join(sessions, file), map[string]string{"sessionId": id, "name": name}); err != nil {
			t.Fatal(err)
		}
	}
	write("valid.json", "a", "reviewer")
	write("same.json", "a", "reviewer")
	write("invalid.json", "a", "bad\nname")
	write("spaces.json", "a", " padded ")
	write("long.json", "a", strings.Repeat("x", 513))
	if got := claudeNames(root); got["a"] != "reviewer" {
		t.Fatalf("invalid metadata hid a valid label: %v", got)
	}
	write("conflict.json", "a", "another reviewer")
	if got := claudeNames(root); got["a"] != "" {
		t.Fatalf("conflicting labels were guessed: %v", got)
	}
	for _, test := range []struct{ name, data string }{
		{"oversize", `{"sessionId":"b","name":"reviewer","padding":"` + strings.Repeat("x", 128*1024) + `"}`},
		{"trailing", `{"sessionId":"b","name":"reviewer"} {}`},
		{"partial", `{"sessionId":"b","name":`},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(sessions, test.name+".json")
			if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			if got := claudeNames(root); got["b"] != "" {
				t.Fatalf("invalid metadata supplied a name: %v", got)
			}
		})
	}
	outside := filepath.Join(t.TempDir(), "metadata.json")
	if err := writeTestJSON(outside, map[string]string{"sessionId": "c", "name": "linked reviewer"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(sessions, "link.json")); err != nil {
		t.Fatal(err)
	}
	if got := claudeNames(root); got["c"] != "" {
		t.Fatalf("symlink metadata supplied a name: %v", got)
	}
	if got := claudeNames(t.TempDir()); len(got) != 0 {
		t.Fatalf("missing metadata supplied names: %v", got)
	}
}
