package install_test

import (
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// A declaration with a malformed addition must not move a panic ahead of
// validating an existing hook event. Preserve the existing refusal first.
func TestMergeRefusesMalformedEventBeforeInspectingAddition(t *testing.T) {
	for _, test := range []struct {
		name     string
		addition any
	}{
		{"empty list", []any{}},
		{"non-list", "invalid addition"},
		{"null", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			path := writeSettings(t, home, ".fixture/hooks.json", `{"hooks":{"stop":"invalid event"}}`)
			declare := func(string, string) ([]install.ConfigFile, error) {
				return []install.ConfigFile{{Path: ".fixture/hooks.json", Draft: "hooks.json", JSON: map[string]any{
					"hooks": map[string]any{"stop": test.addition},
				}}}, nil
			}
			_, err := install.Manage(declare, install.Request{
				Action: "install", Binary: "/fixture/zalki", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true,
			})
			want := path + ": existing hook event is not an array"
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
		})
	}
}
