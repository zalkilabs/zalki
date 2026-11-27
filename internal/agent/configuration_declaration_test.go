package agent_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// configAdapter declares configuration files.
type configAdapter struct {
	plainAdapter
	files []model.ConfigDeclaration
}

func (a configAdapter) Configuration(string, []string) []model.ConfigDeclaration { return a.files }

func ownedEntryDeclaration(mutate func(*model.ConfigDeclaration)) model.ConfigDeclaration {
	file := model.ConfigDeclaration{
		Path: ".app/hooks.json", Draft: "app-hooks.json", OwnedEntry: "zalki",
		JSON: map[string]any{"zalki": map[string]any{"Stop": []any{map[string]any{"command": "run"}}}},
	}
	if mutate != nil {
		mutate(&file)
	}
	return file
}

// A declared owned entry needs a JSON patch with exactly that key and a
// non-empty object as its value; the registry refuses every other shape.
func TestConfigurationChecksADeclaredOwnedEntry(t *testing.T) {
	const refusal = `app declares the owned entry %q, which needs a JSON patch with exactly that key and a non-empty object as its value`
	for _, test := range []struct {
		name  string
		file  model.ConfigDeclaration
		name2 string // the owned entry named in the refusal; empty means the file is valid
	}{
		{"valid owned entry", ownedEntryDeclaration(nil), ""},
		{"no owned entry", ownedEntryDeclaration(func(f *model.ConfigDeclaration) { f.OwnedEntry = "" }), ""},
		{"a TOML fragment", ownedEntryDeclaration(func(f *model.ConfigDeclaration) { f.JSON, f.TOML = nil, "a = 1\n" }), "zalki"},
		{"another key as well", ownedEntryDeclaration(func(f *model.ConfigDeclaration) { f.JSON["other"] = map[string]any{"a": 1} }), "zalki"},
		{"another key instead", ownedEntryDeclaration(func(f *model.ConfigDeclaration) { f.OwnedEntry = "mine" }), "mine"},
		{"an entry that is not an object", ownedEntryDeclaration(func(f *model.ConfigDeclaration) { f.JSON = map[string]any{"zalki": "x"} }), "zalki"},
		{"an empty entry", ownedEntryDeclaration(func(f *model.ConfigDeclaration) { f.JSON = map[string]any{"zalki": map[string]any{}} }), "zalki"},
		{"a name with a space at the edge", ownedEntryDeclaration(func(f *model.ConfigDeclaration) {
			f.OwnedEntry, f.JSON = " zalki", map[string]any{" zalki": map[string]any{"a": 1}}
		}), " zalki"},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, err := agent.NewRegistry(configAdapter{
				plainAdapter: plainAdapter{descriptor: agent.AdapterDescriptor{Kind: "app", Label: "App"}},
				files:        []model.ConfigDeclaration{test.file},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = registry.Configuration("/bin/zalki", nil)
			if test.name2 == "" {
				if err != nil {
					t.Fatalf("a valid declaration was refused: %v", err)
				}
				return
			}
			want := sprintfRefusal(refusal, test.name2)
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}

func sprintfRefusal(format, name string) string {
	return strings.Replace(format, "%q", strconv.Quote(name), 1)
}

// The registry says which client each file belongs to, from the descriptor; an
// adapter cannot say it for itself.
func TestConfigurationNamesTheClientOfEachFile(t *testing.T) {
	file := ownedEntryDeclaration(func(f *model.ConfigDeclaration) {
		f.Client, f.ClientLabel, f.Executable = "other", "Other", "other-cmd"
	})
	registry, err := agent.NewRegistry(configAdapter{
		plainAdapter: plainAdapter{descriptor: agent.AdapterDescriptor{Kind: "app", Label: "App", Executable: "app-cmd"}},
		files:        []model.ConfigDeclaration{file},
	})
	if err != nil {
		t.Fatal(err)
	}
	files, err := registry.Configuration("/bin/zalki", nil)
	if err != nil || len(files) != 1 {
		t.Fatalf("files %v, error %v", files, err)
	}
	if got := files[0]; got.Client != "app" || got.ClientLabel != "App" || got.Executable != "app-cmd" {
		t.Fatalf("client %q, label %q, executable %q; want the descriptor's, not the adapter's", got.Client, got.ClientLabel, got.Executable)
	}
}
