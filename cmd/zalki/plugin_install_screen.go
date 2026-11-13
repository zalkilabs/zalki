package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/zalkilabs/zalki/internal/plugins"
)

// installQuestion is the one question of the install screen. Nothing is
// installed unless the owner answers y.
const installQuestion = "Install for all sessions? [y/N] "

// plainText shows owner-facing text safely: anything not printable is quoted.
func plainText(value string) string {
	if strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return strconv.Quote(value)
	}
	return value
}

// plainLines keeps real line breaks and makes each line safe on its own.
func plainLines(value string) string {
	lines := strings.Split(strings.ReplaceAll(value, "\r\n", "\n"), "\n")
	for i, line := range lines {
		lines[i] = plainText(line)
	}
	return strings.Join(lines, "\n")
}

// installAccessLine says what the plugin gets in every live session.
func installAccessLine(requests plugins.InstallAccess) string {
	var kinds []string
	if requests.Status {
		kinds = append(kinds, "changes = see state and message status, no text")
	}
	if requests.Receive {
		kinds = append(kinds, "receive = sessions may message this plugin, and it may answer them")
	}
	return "Access in every live session: " + strings.Join(kinds, "; ") + "\n"
}

// extendedInstallError refuses send and read at install: they need the new
// install screen. It names the owner-terminal commands that give them now.
func extendedInstallError(name string, requests plugins.InstallAccess) error {
	grant := "zalki plugins grant " + name
	if requests.Status {
		grant += " --status"
	}
	if requests.Receive {
		grant += " --receive"
	}
	if requests.Send {
		grant += " --send SESSION_ID"
	}
	if requests.Read {
		grant += " --read SESSION_ID"
	}
	return fmt.Errorf("plugin install: extended access (send/read) needs the new install screen; not available in this build. To give it now, build the plugin yourself and run in your own terminal:\n"+
		"  zalki plugins add %[1]s --exec /absolute/path/to/the/built/plugin\n"+
		"  zalki plugins configure %[1]s --scope all\n"+
		"  %[2]s\n"+
		"  zalki plugins enable %[1]s", name, grant)
}

// askInstallRaw shows the screen and reads one key: y installs, any other
// key, Enter or closed input cancels.
func askInstallRaw(in io.Reader, out io.Writer, screen string) (bool, error) {
	if _, err := io.WriteString(out, "\x1b[H\x1b[2J"+strings.ReplaceAll(screen+installQuestion, "\n", "\r\n")); err != nil {
		return false, err
	}
	k := readInstallKey(bufio.NewReader(in))
	if _, err := io.WriteString(out, "\r\n"); err != nil {
		return false, err
	}
	return k == "y" || k == "Y", nil
}

// askInstallLines is the plain fallback when raw keys are unavailable.
func askInstallLines(in io.Reader, out io.Writer, screen string) (bool, error) {
	if _, err := io.WriteString(out, screen); err != nil {
		return false, err
	}
	answer, err := askLine(bufio.NewReader(in), out, installQuestion)
	return answer == "y" || answer == "yes", err
}

func askLine(lines *bufio.Reader, out io.Writer, prompt string) (string, error) {
	if _, err := io.WriteString(out, prompt); err != nil {
		return "", err
	}
	line, err := lines.ReadString('\n')
	if cancelled(err) {
		return "", errInstallCancelled
	}
	if err != nil {
		line = "" // Closed input means no, even after a partial line.
	}
	return strings.ToLower(strings.TrimSpace(line)), nil
}

// readInstallKey decodes one key from raw terminal input.
func readInstallKey(r *bufio.Reader) string {
	c, err := r.ReadByte()
	switch {
	case err != nil:
		return "eof"
	case c == 3 || c == 4:
		return "ctrl-c"
	case c == '\r' || c == '\n':
		return "enter"
	case c != 0x1b:
		return string(rune(c))
	}
	// Only ESC '[' or ESC 'O' starts an arrow sequence. Any other byte after
	// ESC stays in the buffer and is the next key, so ESC then y, n, q or
	// Ctrl-C does not lose that key.
	if next, err := r.Peek(1); err != nil || (next[0] != '[' && next[0] != 'O') {
		return "esc"
	}
	_, _ = r.ReadByte() // the '[' or 'O' seen by Peek
	c, err = r.ReadByte()
	if err != nil {
		return "eof"
	}
	return map[byte]string{'A': "up", 'B': "down", 'C': "right", 'D': "left"}[c]
}
