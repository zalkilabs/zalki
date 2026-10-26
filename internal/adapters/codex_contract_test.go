package adapters

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestRecordedCodexHookAndMetadataContract(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "bus", "testdata", "clients", "codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Version       string          `json:"version"`
		VersionOutput string          `json:"version_output"`
		Hook          json.RawMessage `json:"hook"`
		NameRecord    json.RawMessage `json:"name_record"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	adapter, err := Builtin().Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	var hook bus.HookInput
	if err := json.Unmarshal(fixture.Hook, &hook); err != nil {
		t.Fatal(err)
	}
	normalizer, ok := adapter.(interface {
		NormalizeHook(bus.HookInput) (bus.HookInput, error)
	})
	if !ok {
		t.Fatal("codex adapter has no lifecycle normalizer")
	}
	hook, err = normalizer.NormalizeHook(hook)
	if err != nil || hook.SessionID != "00000000-0000-4000-8000-000000000001" || hook.CWD != "/fixture/workspace" || adapter.Descriptor().Hooks[hook.Event] != "SessionStart" {
		t.Fatal("recorded native hook no longer normalizes", err)
	}
	if fixture.Version != "unknown" && clientVersionPattern.FindString(fixture.VersionOutput) != fixture.Version {
		t.Fatal("native version output no longer parses")
	}
	root := t.TempDir()
	var record bytes.Buffer
	if err := json.Compact(&record, fixture.NameRecord); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), append(record.Bytes(), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	names := adapter.(interface {
		LookupName(bus.Session, map[string]map[string]string) (string, string)
	})
	name, _ := names.LookupName(bus.Session{ID: "codex:" + hook.SessionID, Kind: "codex", NativeID: hook.SessionID, NativeConfigDir: root}, map[string]map[string]string{})
	if name != "Contract review" {
		t.Fatalf("native metadata contract changed: got %q", name)
	}
}

func TestCodexNamesFollowAppendOnlyIndexRules(t *testing.T) {
	write := func(root string, lines ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "session_index.jsonl"), []byte(joinLines(lines)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	write(root, `{"id":"a","thread_name":"first"}`, `{"id":"b","thread_name":"other"}`, `{"id":"a","thread_name":"renamed"}`)
	if got := codexNames(root); got["a"] != "renamed" || got["b"] != "other" {
		t.Fatalf("a later row must supersede an earlier one: %v", got)
	}
	write(root, `{"id":"a","thread_name":"first"}`, `{"id":"a","thread_name":""}`)
	if got := codexNames(root); got["a"] != "" {
		t.Fatalf("an empty name must supersede a previous label: %v", got)
	}
	write(root, `{"id":"a","thread_name":"first"}`, `{"id":`)
	if got := codexNames(root); got != nil {
		t.Fatalf("an interrupted append must not resurrect old labels: %v", got)
	}
	if got := codexNames(t.TempDir()); got != nil {
		t.Fatalf("a missing index produced names: %v", got)
	}
	link := t.TempDir()
	if err := os.Symlink(filepath.Join(root, "session_index.jsonl"), filepath.Join(link, "session_index.jsonl")); err != nil {
		t.Fatal(err)
	}
	if got := codexNames(link); got != nil {
		t.Fatalf("a non-regular index was read: %v", got)
	}
}

func joinLines(lines []string) string {
	var out bytes.Buffer
	for _, line := range lines {
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return out.String()
}
