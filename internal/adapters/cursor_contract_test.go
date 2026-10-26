package adapters

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

type clientContract struct {
	Kind          string          `json:"kind"`
	Version       string          `json:"version"`
	VersionOutput string          `json:"version_output"`
	Hook          json.RawMessage `json:"hook"`
	NameRecord    json.RawMessage `json:"name_record"`
	Frame         map[string]any  `json:"frame"`
	QueueArgs     []string        `json:"queue_args"`
}

func TestCursorMetadataRejectsOversizedAndLinkedFiles(t *testing.T) {
	id := newTestID()
	root := t.TempDir()
	dir := filepath.Join(root, "chats", "workspace", id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "meta.json")
	for _, data := range []string{
		`{"schemaVersion":1,"title":"reviewer","padding":"` + strings.Repeat("x", 128*1024) + `"}`,
		`{"schemaVersion":1,"title":"reviewer"} {}`,
		`{"schemaVersion":1,"title":`,
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if got := cursorNameInRoots([]string{root}, id); got != "" {
			t.Fatalf("invalid metadata supplied a title: %q", got)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(t.TempDir(), "meta.json")
	if err := os.WriteFile(other, []byte(`{"schemaVersion":1,"title":"linked reviewer"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, path); err != nil {
		t.Fatal(err)
	}
	if got := cursorNameInRoots([]string{root}, id); got != "" {
		t.Fatalf("linked metadata supplied a title: %q", got)
	}
}

func TestRecordedCursorHookAndMetadataContract(t *testing.T) {
	paths, err := filepath.Glob("../bus/testdata/clients/*.json")
	if err != nil || len(paths) < 3 {
		t.Fatal("missing client contract fixtures", err)
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fixture clientContract
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatal(err)
		}
		if fixture.Kind != "cursor" {
			continue // Extracted adapters have real contract tests in internal/adapters.
		}
		t.Run(fixture.Kind, func(t *testing.T) {
			adapter, err := Builtin().Lookup(fixture.Kind)
			if err != nil {
				t.Fatal(err)
			}
			var hook bus.HookInput
			if err := json.Unmarshal(fixture.Hook, &hook); err != nil {
				t.Fatal(err)
			}
			hooks := adapter.(interface {
				NormalizeHook(bus.HookInput) (bus.HookInput, error)
			})
			hook, err = hooks.NormalizeHook(hook)
			if err != nil || hook.SessionID != "00000000-0000-4000-8000-000000000001" || hook.CWD != "/fixture/workspace" || adapter.Descriptor().Hooks[hook.Event] != "SessionStart" {
				t.Fatal("recorded native hook no longer normalizes", err)
			}
			if fixture.Version != "unknown" && clientVersionPattern.FindString(fixture.VersionOutput) != fixture.Version {
				t.Fatal("native version output no longer parses")
			}
			if len(fixture.NameRecord) == 0 {
				return
			}
			root := t.TempDir()
			var file string
			switch fixture.Kind {
			case "cursor":
				file = filepath.Join(root, "chats", "fixture", hook.SessionID, "meta.json")
			}
			if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
				t.Fatal(err)
			}
			var record bytes.Buffer
			if err := json.Compact(&record, fixture.NameRecord); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, append(record.Bytes(), '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			name, _ := adapter.(interface {
				LookupName(bus.Session, map[string]map[string]string) (string, string)
			}).LookupName(bus.Session{ID: fixture.Kind + ":" + hook.SessionID, Kind: fixture.Kind, NativeID: hook.SessionID, NativeConfigDir: root}, map[string]map[string]string{})
			if name != "Contract review" {
				t.Fatalf("native metadata contract changed: got %q", name)
			}
		})
	}
}
