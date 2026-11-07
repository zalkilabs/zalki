package bus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

type hintAdapter struct {
	stubCodexAdapter
	hint string
}

func (a hintAdapter) RecoveryHint(SessionDiagnostic) string { return a.hint }

func TestRecoveryHintPrefersCommonStepsAndBoundsAdapterHints(t *testing.T) {
	adapter := hintAdapter{hint: "Client-specific step."}
	if hint := recoveryHint(SessionDiagnostic{}, adapter); hint != adapter.hint {
		t.Fatalf("adapter hint was not used: %q", hint)
	}
	for _, d := range []SessionDiagnostic{{Retired: true}, {Liveness: "exited"}, {HookBuildStatus: "outdated"}} {
		if hint := recoveryHint(d, adapter); hint == "" || hint == adapter.hint {
			t.Fatalf("common recovery step was not preferred for %+v: %q", d, hint)
		}
	}
	if hint := recoveryHint(SessionDiagnostic{}, stubCodexAdapter{}); hint != "" {
		t.Fatalf("adapter without hints produced %q", hint)
	}
	for _, invalid := range []string{"line\nbreak", "\x1b[31mred", strings.Repeat("x", 513), "\xff"} {
		if hint := recoveryHint(SessionDiagnostic{}, hintAdapter{hint: invalid}); hint != "" {
			t.Fatalf("malformed adapter hint was printed: %q", hint)
		}
	}
}

func TestClientVersionProbeAndUnknownVersionDiagnostics(t *testing.T) {
	for _, test := range []struct{ output, want string }{
		{"codex-cli 0.163.0", "0.163.0"}, {"2.1.308 (Claude Code)", "2.1.308"},
		{"2026.10.22-27dcf5f", "2026.10.22-27dcf5f"}, {"unrecognized output", ""},
	} {
		path := filepath.Join(t.TempDir(), "client")
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+test.output+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if got := probeClientVersion(t.Context(), path); got != test.want {
			t.Fatalf("got version %q want %q", got, test.want)
		}
		diagnostic := AgentDiagnostic{Kind: "codex", Binary: path, Found: true}
		annotateClientCompatibility(t.Context(), &diagnostic)
		if test.want == "0.163.0" && diagnostic.Compatibility != "fixture_covered" {
			t.Fatal("known fixture version was not identified")
		}
		if test.want != "" && test.want != "0.163.0" && diagnostic.Compatibility != "unverified_version" {
			t.Fatal("unknown version was treated as supported")
		}
		if diagnostic.RecoveryHint == "" {
			t.Fatal("diagnostics omitted a next step")
		}
	}
	buffer := versionBuffer{}
	_, _ = buffer.Write([]byte(strings.Repeat("x", 4096)))
	if buffer.buffer.Len() != 1024 {
		t.Fatal("version output was not bounded")
	}
}

type compatibilityStub struct {
	stubCodexAdapter
	status, hint string
}

func (a compatibilityStub) Compatibility(string) (string, string) { return a.status, a.hint }

func TestAdapterVersionEvidenceDoesNotOverrideUnavailableClientsOrMalformedHints(t *testing.T) {
	for _, test := range []struct {
		found   bool
		version string
		adapter agent.Adapter
		want    string
	}{
		{true, "2.1.316", compatibilityStub{status: "live_verified", hint: "Historical smoke evidence; no per-message ACK."}, "live_verified"},
		{false, "2.1.316", compatibilityStub{status: "live_verified", hint: "Known"}, "original"},
		{true, "", compatibilityStub{status: "live_verified", hint: "Known"}, "original"},
		{true, "2.1.316", compatibilityStub{status: "live_verified", hint: "bad\nhint"}, "original"},
		{true, "2.1.316", compatibilityStub{status: "bad status", hint: "Known"}, "original"},
		{true, "2.1.316", stubCodexAdapter{}, "original"},
	} {
		diagnostic := AgentDiagnostic{Found: test.found, Version: test.version, Compatibility: "original", RecoveryHint: "Original hint."}
		annotateAdapterCompatibility(&diagnostic, test.adapter)
		if diagnostic.Compatibility != test.want {
			t.Fatalf("compatibility=%s want=%s", diagnostic.Compatibility, test.want)
		}
		if test.want == "original" && diagnostic.RecoveryHint != "Original hint." {
			t.Fatal("unavailable or invalid evidence replaced the common next step")
		}
	}
}
