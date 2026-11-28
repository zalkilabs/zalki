package codex

import (
	"fmt"

	"github.com/zalkilabs/zalki/internal/agent"
)

var _ agent.CheckAdapter = Adapter{}

// Checks gives the doctor's checklist lines about Codex hook trust, from the
// same offline comparison as the doctor section. A hook that is not trusted, was
// changed since it was trusted, or is turned off does not run, so Zalki cannot
// register or deliver for that thread: a problem. A trust state that cannot be
// compared (another Codex version, an unreadable file) is only a note.
func (a Adapter) Checks(in agent.DiagnosticInput) []agent.Check {
	if !in.Found {
		return nil
	}
	return codexHookChecks(a.diagnoseCodexHooks(in))
}

func codexHookChecks(d codexHooksDiagnostic) []agent.Check {
	counts := map[string]int{}
	for _, row := range d.Hooks {
		counts[row.Status]++
	}
	total := len(d.Hooks)
	var checks []agent.Check
	if n := counts["not_trusted"]; n > 0 {
		checks = append(checks, agent.Check{Name: hooksOf(n, total, "is not trusted yet", "are not trusted yet"), Status: "problem", Fix: "open /hooks in Codex and trust the zalki hooks, then send a message in each thread"})
	}
	if n := counts["modified"]; n > 0 {
		checks = append(checks, agent.Check{Name: hooksOf(n, total, "was changed since it was trusted", "were changed since they were trusted"), Status: "problem", Fix: "open /hooks in Codex, review the zalki hooks and trust them again"})
	}
	if n := counts["disabled"]; n > 0 {
		checks = append(checks, agent.Check{Name: hooksOf(n, total, "is turned off", "are turned off"), Status: "problem", Fix: "turn the zalki hooks on in /hooks in Codex"})
	}
	if total == 0 || counts["cannot_verify"] > 0 {
		checks = append(checks, agent.Check{Name: "Codex: hook trust could not be checked", Status: "note", Fix: "open /hooks in Codex and check that the zalki hooks are trusted"})
	}
	if total > 0 && counts["trusted"] == total {
		checks = append(checks, agent.Check{Name: fmt.Sprintf("Codex: all %d zalki hooks are trusted", total), Status: "ok"})
	}
	return checks
}

// hooksOf is "Codex: 1 of its 7 zalki hooks is ...", "Codex: 2 of its 7 zalki hooks are ..." or, for all of them, "Codex: its 7 zalki hooks are ...".
func hooksOf(n, total int, one, many string) string {
	verb := many
	if n == 1 {
		verb = one
	}
	if n == total {
		if total == 1 {
			return "Codex: its zalki hook " + one
		}
		return fmt.Sprintf("Codex: its %d zalki hooks %s", total, many)
	}
	return fmt.Sprintf("Codex: %d of its %d zalki hooks %s", n, total, verb)
}
