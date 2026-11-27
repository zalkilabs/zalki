package main

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// A failure prints what happened and the next step on two lines; the cause is
// reachable but never printed. Other errors keep the one-line form.
func TestErrorLinesPrintTheTwoLineFailureAndHideTheCause(t *testing.T) {
	cause := errors.New("lstat /secret/path: no such file")
	err := failure("Zalki could not do it.", "run zalki doctor.", cause)
	if got := errorLines(err); got != "Failed: Zalki could not do it.\nNext: run zalki doctor." {
		t.Fatalf("failure lines = %q", got)
	}
	if !errors.Is(err, cause) || strings.Contains(errorLines(err), "secret") {
		t.Fatal("the cause was printed or cannot be reached")
	}
	if got := errorLines(errors.New("plain")); got != "zalki: plain" {
		t.Fatalf("plain error = %q", got)
	}
	if got := errorLines(errors.Join(errors.New("other"), err)); !strings.HasPrefix(got, "Failed:") {
		t.Fatalf("a joined failure was not found: %q", got)
	}
}

// The mark text never depends on the terminal; only the colour does, and
// NO_COLOR or TERM=dumb switch the colour off.
func TestMarkTextColoursOnlyOnATerminalWithoutNoColor(t *testing.T) {
	old := outputIsTerminal
	t.Cleanup(func() { outputIsTerminal = old })
	var sink io.Writer = &bytes.Buffer{}
	for _, tc := range []struct {
		name, noColor, term string
		terminal            bool
		want                string
	}{
		{"pipe", "", "xterm", false, "[note]"},
		{"terminal", "", "xterm", true, "\x1b[33m[note]\x1b[0m"},
		{"NO_COLOR set", "1", "xterm", true, "[note]"},
		{"TERM dumb", "", "dumb", true, "[note]"},
	} {
		t.Setenv("NO_COLOR", tc.noColor)
		t.Setenv("TERM", tc.term)
		terminal := tc.terminal
		outputIsTerminal = func(io.Writer) bool { return terminal }
		if got := markText(sink, markNote); got != tc.want {
			t.Fatalf("%s: mark = %q, want %q", tc.name, got, tc.want)
		}
	}
	if got := markText(sink, "unknown"); got != "[unknown]" {
		t.Fatalf("a mark without a colour = %q", got)
	}
}

func TestPrintErrorWritesTheLinesMainShows(t *testing.T) {
	var out bytes.Buffer
	printError(&out, failure("It broke.", "try again.", errors.New("hidden")))
	printError(&out, errors.New("plain"))
	if out.String() != "Failed: It broke.\nNext: try again.\nzalki: plain\n" {
		t.Fatalf("printed %q", out.String())
	}
}

func TestCountAndJoinWords(t *testing.T) {
	if countOf(1, "file", "files") != "1 file" || countOf(2, "file", "files") != "2 files" || countOf(0, "file", "files") != "0 files" {
		t.Fatal("countOf")
	}
	for in, want := range map[string]string{"": "", "a": "a", "a|b": "a and b", "a|b|c": "a, b and c"} {
		var parts []string
		if in != "" {
			parts = strings.Split(in, "|")
		}
		if got := joinParts(parts); got != want {
			t.Fatalf("joinParts(%q) = %q, want %q", in, got, want)
		}
	}
}
