package install

import (
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"
)

func checkClientRoot(name, path string) error {
	if !utf8.ValidString(path) || strings.IndexFunc(path, func(r rune) bool { return !unicode.IsGraphic(r) }) >= 0 {
		return fmt.Errorf("%s: invalid path. Use a path without control characters (including newlines)", name)
	}
	return nil
}

// CheckLogFile refuses sinks that would replace settings before the command runs.
// It also validates native client roots before their paths can enter diagnostics.
func CheckLogFile(declare Declare, logFile, state, home string) error {
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		if err := checkClientRoot(name, os.Getenv(name)); err != nil {
			return err
		}
	}
	// Uninstall uses recorded targets, so its caller supplies no declaration.
	if logFile == "-" || logFile == "" || declare == nil {
		return nil
	}
	files, err := declare("/zalki", state)
	if err != nil {
		return err
	}
	for _, file := range files {
		target, err := file.target(home)
		if err != nil {
			return err
		}
		same, err := sameTargetFile(logFile, target)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("--log-file points at a file Zalki edits for %s (%q); choose another log file", file.Draft, target)
		}
	}
	return nil
}
