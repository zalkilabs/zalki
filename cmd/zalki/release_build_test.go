package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Go ignores -X for a missing symbol, so a wrong path in the release config
// builds without error and ships the default version. This builds the real
// binary with the variable named in .goreleaser.yaml and reads the version
// back from the built binary.
func TestReleaseLinkerFlagSetsTheReportedVersion(t *testing.T) {
	config, err := os.ReadFile("../../.goreleaser.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var target string
	fields := strings.Fields(string(config))
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == "-X" {
			target, _, _ = strings.Cut(fields[i+1], "=")
		}
	}
	if target == "" {
		t.Fatal("no -X flag in .goreleaser.yaml")
	}
	dir := t.TempDir()
	binary := filepath.Join(dir, "zalki")
	build := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-ldflags", "-X "+target+"=9.8.7", "-o", binary, ".")
	if raw, err := build.CombinedOutput(); err != nil {
		t.Fatal("build with the release flag", string(raw), err)
	}
	run := exec.CommandContext(t.Context(), binary, "version")
	run.Env = []string{"HOME=" + dir, "ZALKI_STATE=" + filepath.Join(dir, "state"), "ZALKI_CONFIG=" + filepath.Join(dir, "config.json")}
	raw, err := run.Output()
	if err != nil {
		t.Fatal("run the built binary", err)
	}
	if !regexp.MustCompile(`^9\.8\.7\+[0-9a-f]{12}\n$`).Match(raw) {
		t.Fatalf("built binary reports %q, want 9.8.7 and a build id", raw)
	}
}
