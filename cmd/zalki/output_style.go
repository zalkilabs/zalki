package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// The words a command prints for people. A failure says what happened and
// the one next step, each on its own line; a result says what happened.
// Full details stay in --json and in the log file.

// nextStepError is an error that is printed as two lines: what failed and what
// to do next. The cause stays out of the printed text; errors.Is and
// errors.As still reach it, and the command log records its kind.
type nextStepError struct {
	what, next string
	cause      error
}

// failure builds a nextStepError. what and next are plain sentences without
// paths or internal codes; cause may be nil.
func failure(what, next string, cause error) error {
	return &nextStepError{what: what, next: next, cause: cause}
}

func (f *nextStepError) Error() string { return "Failed: " + f.what + "\nNext: " + f.next }
func (f *nextStepError) Unwrap() error { return f.cause }

// errorLines is what main prints on stderr for a command error.
func errorLines(err error) string {
	var f *nextStepError
	if errors.As(err, &f) {
		return f.Error()
	}
	return "zalki: " + err.Error()
}

// printError writes a command error to w, as main does on stderr.
func printError(w io.Writer, err error) { _, _ = fmt.Fprintln(w, errorLines(err)) }

// jsonFlagHelp is the help text of --json on every command that prints data.
const jsonFlagHelp = "print the result as JSON, for scripts"

// markNote is the text mark of a finding that is advice, not a failure.
const markNote = "note"

var markColour = map[string]string{markNote: "33"} // yellow

// outputIsTerminal says whether w is a terminal; tests replace it.
var outputIsTerminal = writerIsTerminal

// colourOn is true only on a terminal, with NO_COLOR unset or empty and TERM
// not "dumb".
func colourOn(w io.Writer) bool {
	return os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb" && outputIsTerminal(w)
}

// markText is the mark "[note]" for w; only the colour depends on the
// terminal, never the text.
func markText(w io.Writer, mark string) string {
	text := "[" + mark + "]"
	if code, ok := markColour[mark]; ok && colourOn(w) {
		return "\x1b[" + code + "m" + text + "\x1b[0m"
	}
	return text
}

// countOf is "1 record" or "3 records".
func countOf(n int, singular, plural string) string {
	if n == 1 {
		return "1 " + singular
	}
	return fmt.Sprintf("%d %s", n, plural)
}

// joinParts joins non-empty parts as "a, b and c".
func joinParts(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}
