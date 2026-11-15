package bus

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

// releaseMarker gives the import path of this package.
type releaseMarker struct{}

// Go ignores -X for a missing symbol, so a wrong path in the release
// config would ship the default Version without any build error.
func TestReleaseLinkerFlagNamesTheVersionVariable(t *testing.T) {
	if Version == "" {
		t.Fatal("Version has no default value")
	}
	config, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var fields []string
	for line := range strings.Lines(string(config)) {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		line, _, _ = strings.Cut(line, " #")
		fields = append(fields, strings.Fields(line)...)
	}
	var targets []string
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "-X" {
			target, _, _ := strings.Cut(fields[i+1], "=")
			targets = append(targets, target)
		}
	}
	if len(targets) != 1 {
		t.Fatalf("found %d -X flags, want 1", len(targets))
	}
	if want := reflect.TypeOf(releaseMarker{}).PkgPath() + ".Version"; targets[0] != want {
		t.Fatalf("release linker flag target = %q, want %q", targets[0], want)
	}
}
