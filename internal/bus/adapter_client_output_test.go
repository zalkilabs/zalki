package bus

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func versionOutputExecutable(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "client")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestClientVersionProbeBoundsExecutableOutput(t *testing.T) {
	for _, test := range []struct{ name, script, want string }{
		{"version_after_limit", "printf '%s' '" + strings.Repeat("x", 1024) + " 9.8.7'", ""},
		{"version_at_limit", "printf '%s' '" + strings.Repeat("x", 1019) + "9.8.7'", "9.8.7"},
		{"normal_output", "printf 'client 0.163.0'", "0.163.0"},
		{"stderr_only", "printf '9.8.7' >&2", ""},
		{"command_failure", "printf '9.8.7'; exit 1", ""},
		{"unrecognized_output", "printf 'no version'", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := versionOutputExecutable(t, test.script)
			if got := probeClientVersion(t.Context(), path); got != test.want {
				t.Fatalf("version=%q want=%q", got, test.want)
			}
		})
	}
}

func TestVersionBufferRetainsOnlyExecutablePrefix(t *testing.T) {
	prefix := "client 0.163.0" + strings.Repeat("x", 4096)
	path := versionOutputExecutable(t, "printf '%s' '"+prefix+" 9.8.7'")
	var output versionBuffer
	command := exec.CommandContext(t.Context(), path, "--version")
	command.Stdout = &output
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if got := output.buffer.String(); got != prefix[:1024] {
		t.Fatalf("retained %d bytes, want exact first 1024 bytes", len(got))
	}
}
