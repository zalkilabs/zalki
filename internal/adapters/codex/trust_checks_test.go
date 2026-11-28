package codex

import (
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func rows(statuses ...string) codexHooksDiagnostic {
	d := codexHooksDiagnostic{Hooks: []codexHookDiagnostic{}}
	for _, s := range statuses {
		d.Hooks = append(d.Hooks, codexHookDiagnostic{Status: s})
	}
	return d
}

func checkNames(checks []agent.Check) string {
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Status+": "+c.Name)
	}
	return strings.Join(names, " | ")
}

// A hook that will not run is a problem with the one fix; hooks that cannot be
// compared are only a note; all trusted is one ok line.
func TestCodexHookChecksByTrustState(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rows  codexHooksDiagnostic
		want  string
		fixes []string
	}{
		{"all trusted", rows("trusted", "trusted", "trusted"), "ok: Codex: all 3 zalki hooks are trusted", nil},
		{"not trusted", rows("trusted", "not_trusted", "not_trusted"), "problem: Codex: 2 of its 3 zalki hooks are not trusted yet", []string{"open /hooks in Codex and trust the zalki hooks"}},
		{"all not trusted", rows("not_trusted", "not_trusted", "not_trusted"), "problem: Codex: its 3 zalki hooks are not trusted yet", nil},
		{"the only hook", rows("not_trusted"), "problem: Codex: its zalki hook is not trusted yet", nil},
		{"one not trusted", rows("not_trusted", "trusted"), "problem: Codex: 1 of its 2 zalki hooks is not trusted yet", []string{"trust the zalki hooks"}},
		{"modified", rows("trusted", "modified"), "problem: Codex: 1 of its 2 zalki hooks was changed since it was trusted", []string{"review the zalki hooks and trust them again"}},
		{"two modified", rows("modified", "modified", "trusted"), "problem: Codex: 2 of its 3 zalki hooks were changed since they were trusted", nil},
		{"disabled", rows("disabled", "trusted"), "problem: Codex: 1 of its 2 zalki hooks is turned off", []string{"turn the zalki hooks on"}},
		{"cannot verify", rows("cannot_verify", "cannot_verify"), "note: Codex: hook trust could not be checked", []string{"check that the zalki hooks are trusted"}},
		{"no rows", rows(), "note: Codex: hook trust could not be checked", []string{"open /hooks"}},
		{"mixed", rows("not_trusted", "cannot_verify"), "problem: Codex: 1 of its 2 zalki hooks is not trusted yet | note: Codex: hook trust could not be checked", nil},
	} {
		got := codexHookChecks(tc.rows)
		if names := checkNames(got); names != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, names, tc.want)
		}
		for i, fix := range tc.fixes {
			if !strings.Contains(got[i].Fix, fix) {
				t.Fatalf("%s: fix %q lacks %q", tc.name, got[i].Fix, fix)
			}
		}
		for _, c := range got {
			if c.Status != "ok" && c.Fix == "" {
				t.Fatalf("%s: a finding without a fix: %+v", tc.name, c)
			}
		}
	}
}

// Without the client nothing is said; for a version whose hashes are unknown the
// real comparison says "could not be checked", as a note.
func TestCodexChecksNeedTheClientAndSayCannotCheckForOtherVersions(t *testing.T) {
	a := New(false, "fixture.json")
	if got := a.Checks(agent.DiagnosticInput{Found: false}); len(got) != 0 {
		t.Fatalf("a client that is not installed gave %+v", got)
	}
	home := t.TempDir()
	t.Setenv("CODEX_HOME", "")
	got := a.Checks(agent.DiagnosticInput{Home: home, Found: true, Version: "9.9.9"})
	if len(got) != 1 || got[0].Status != "note" || got[0].Name != "Codex: hook trust could not be checked" {
		t.Fatalf("checks = %+v", got)
	}
}
