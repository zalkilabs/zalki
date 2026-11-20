package bus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

type hintAdapter struct {
	stubCodexAdapter
	hint string
}

func (a hintAdapter) RecoveryHint(model.SessionDiagnostic) string { return a.hint }

func TestRecoveryHintPrefersCommonStepsAndBoundsAdapterHints(t *testing.T) {
	adapter := hintAdapter{hint: "Client-specific step."}
	if hint := recoveryHint(model.SessionDiagnostic{}, adapter); hint != adapter.hint {
		t.Fatalf("adapter hint was not used: %q", hint)
	}
	for _, d := range []model.SessionDiagnostic{{Retired: true}, {Liveness: "exited"}, {HookBuildStatus: "outdated"}} {
		if hint := recoveryHint(d, adapter); hint == "" || hint == adapter.hint {
			t.Fatalf("common recovery step was not preferred for %+v: %q", d, hint)
		}
	}
	if hint := recoveryHint(model.SessionDiagnostic{}, stubCodexAdapter{}); hint != "" {
		t.Fatalf("adapter without hints produced %q", hint)
	}
	for _, invalid := range []string{"line\nbreak", "\x1b[31mred", strings.Repeat("x", 513), "\xff"} {
		if hint := recoveryHint(model.SessionDiagnostic{}, hintAdapter{hint: invalid}); hint != "" {
			t.Fatalf("malformed adapter hint was printed: %q", hint)
		}
	}
}

// slowVersionProbes gives the /bin/sh fake clients time to start under a
// loaded full race; the production 5 s bound is unchanged.
func slowVersionProbes(t *testing.T) {
	t.Helper()
	before := clientVersionProbeTimeout
	clientVersionProbeTimeout = 30 * time.Second
	t.Cleanup(func() { clientVersionProbeTimeout = before })
}

func TestClientVersionProbeAndUnknownVersionDiagnostics(t *testing.T) {
	slowVersionProbes(t)
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
		if diagnostic.Version != test.want {
			t.Fatalf("diagnostic version %q want %q", diagnostic.Version, test.want)
		}
		if test.want == "" && (diagnostic.Compatibility != "version_unavailable" || diagnostic.RecoveryHint == "") {
			t.Fatalf("unreadable version was not reported with a next step: %+v", diagnostic)
		}
		if test.want != "" && (diagnostic.Compatibility != "installed" || diagnostic.RecoveryHint != "") {
			t.Fatalf("a readable version was judged or given a hint: %+v", diagnostic)
		}
	}
	missing := AgentDiagnostic{Kind: "codex"}
	annotateClientCompatibility(t.Context(), &missing)
	if missing.Compatibility != "not_installed" || missing.RecoveryHint == "" || missing.Version != "" {
		t.Fatalf("missing client was not reported: %+v", missing)
	}
	buffer := versionBuffer{}
	_, _ = buffer.Write([]byte(strings.Repeat("x", 4096)))
	if buffer.buffer.Len() != 1024 {
		t.Fatal("version output was not bounded")
	}
}
