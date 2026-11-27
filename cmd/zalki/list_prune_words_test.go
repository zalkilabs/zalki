package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestListWordsShowOneLinePerSessionAndNoRawControlText(t *testing.T) {
	fake := &fakeDiagnostics{sessions: []model.Session{
		{ID: "claude:aaaa", Name: "Zara", State: model.StateBusy, Task: "fix the login bug"},
		{ID: "codex:bbbb", State: model.StateApproval, Task: "x\x1b[31m" + strings.Repeat("y", 80)},
		{ID: "cursor:cccc", Name: "Old", State: model.StateReady, Liveness: model.LivenessExited},
		{ID: "claude:dddd", Name: "Gone", State: model.StateReady, Retired: true},
	}}
	cmd, out, _ := portCommand()
	if err := runList(cmd, fake, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "SESSION") {
		t.Fatalf("lines = %q", lines)
	}
	for i, want := range []string{"claude:aaaa", "Zara", "busy", "fix the login bug"} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("line 1 lacks %q (%d): %q", want, i, lines[1])
		}
	}
	if !strings.Contains(lines[2], "needs approval") || strings.Contains(out.String(), "\x1b") || !strings.Contains(lines[2], "...") {
		t.Fatalf("approval line or control text: %q", lines[2])
	}
	if !strings.Contains(lines[3], "exited") || !strings.Contains(lines[4], "retired") {
		t.Fatalf("exited/retired words: %q %q", lines[3], lines[4])
	}
	if strings.Contains(out.String(), "{") {
		t.Fatal("JSON in the words output")
	}
}

func TestListWordsForNoSessionsSayWhatToDoNext(t *testing.T) {
	cmd, out, _ := portCommand()
	if err := runList(cmd, &fakeDiagnostics{sessions: []model.Session{}}, false); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.HasPrefix(got, "No sessions are registered.\nNext: ") || strings.Count(got, "\n") != 2 {
		t.Fatalf("empty list = %q", got)
	}
}

func TestListFailureIsTwoLinesWithTheCauseBehindIt(t *testing.T) {
	fake := &fakeDiagnostics{listErr: errors.New("/very/private/path broke")}
	cmd, out, _ := portCommand()
	err := runList(cmd, fake, false)
	if !errors.Is(err, fake.listErr) || out.Len() != 0 || strings.Contains(errorLines(err), "private") || !strings.HasPrefix(errorLines(err), "Failed: ") || !strings.Contains(errorLines(err), "\nNext: ") {
		t.Fatalf("error %v output %q", err, out.String())
	}
}

func TestListSkippedRecordsWarnWithAMarkAndANextLine(t *testing.T) {
	for n, want := range map[int]string{1: "[note] 1 session record cannot be read, so it is not listed.\n", 3: "[note] 3 session records cannot be read, so they are not listed.\n"} {
		cmd, _, errOut := portCommand()
		if err := runList(cmd, &fakeDiagnostics{sessions: []model.Session{}, skipped: n}, false); err != nil {
			t.Fatal(err)
		}
		if got := errOut.String(); got != want+"Next: run zalki doctor to see why.\n" {
			t.Fatalf("warning for %d = %q", n, got)
		}
	}
}

func TestPruneWordsSayWhatHappenedAndHowToApply(t *testing.T) {
	item := bus.PruneItem{ID: "claude:a", Reason: "owner_exited"}
	report := bus.PruneReport{Sessions: []bus.PruneItem{item, item}, DeletedSessions: []bus.PruneItem{item}, Runtimes: []string{"r"}, DeletedLocks: []string{"l"}}
	for _, tc := range []struct {
		name   string
		report bus.PruneReport
		want   string
	}{
		{"nothing", bus.PruneReport{}, "Nothing to clean up.\n"},
		{"nothing in a preview", bus.PruneReport{DryRun: true}, "Nothing to clean up.\n"},
		{"done", report, "Done: retired 2 stale sessions, deleted 1 old retired record and removed 2 leftover files. Message history was kept.\n"},
		{"preview", withDryRun(report), "Preview only, nothing changed. It would retire 2 stale sessions, delete 1 old retired record and remove 2 leftover files.\nNext: to apply, run the same command without --dry-run.\n"},
		{"one file", bus.PruneReport{DeletedFlood: []string{"f"}}, "Done: removed 1 leftover file. Message history was kept.\n"},
	} {
		cmd, out, _ := portCommand()
		if err := printPrune(cmd, tc.report); err != nil || out.String() != tc.want {
			t.Fatalf("%s: %q err %v", tc.name, out.String(), err)
		}
	}
}

func withDryRun(report bus.PruneReport) bus.PruneReport {
	report.DryRun = true
	return report
}

func TestPruneFailureIsTwoLinesAndPrintsNothing(t *testing.T) {
	fake := &fakeDiagnostics{pruneErr: errors.New("/very/private/path locked")}
	cmd, out, _ := portCommand()
	err := runPrune(cmd, fake, false, nil, time.Hour, false)
	if !errors.Is(err, fake.pruneErr) || out.Len() != 0 || strings.Contains(errorLines(err), "private") || !strings.HasPrefix(errorLines(err), "Failed: ") {
		t.Fatalf("error %v output %q", err, out.String())
	}
}

// The task cut is exact: up to 60 characters stay whole; a longer one keeps 57
// characters and "..." (60 in all); a character is never cut in the middle.
func TestShortenCutsAtExactlySixtyCharacters(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"short", "fix it", "fix it"},
		{"exactly 60", strings.Repeat("a", 60), strings.Repeat("a", 60)},
		{"61", strings.Repeat("a", 61), strings.Repeat("a", 57) + "..."},
		{"long", strings.Repeat("a", 200), strings.Repeat("a", 57) + "..."},
		{"multi-byte", strings.Repeat("é", 70), strings.Repeat("é", 57) + "..."},
	} {
		if got := shorten(tc.in, listTaskWidth); got != tc.want {
			t.Fatalf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	if listTaskWidth != 60 {
		t.Fatalf("the list task width is %d, want 60", listTaskWidth)
	}
	fake := &fakeDiagnostics{sessions: []model.Session{{ID: "claude:aaaa", State: model.StateBusy, Task: strings.Repeat("b", 80)}}}
	cmd, out, _ := portCommand()
	if err := runList(cmd, fake, false); err != nil || !strings.Contains(out.String(), "  "+strings.Repeat("b", 57)+"...\n") {
		t.Fatalf("list output %q err %v", out.String(), err)
	}
}
