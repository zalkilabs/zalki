package main

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

// installKind is one kind of access the manifest asked for, per session.
type installKind struct {
	label, help string
	field       func(*pluginfeed.PluginInstallGrants) *[]string
}

var installKinds = []struct {
	installKind
	requested func(plugins.InstallRequests) bool
}{
	{installKind{"changes", "see message changes", func(g *pluginfeed.PluginInstallGrants) *[]string { return &g.Sessions }}, func(r plugins.InstallRequests) bool { return r.Feed }},
	{installKind{"read", "read message bodies", func(g *pluginfeed.PluginInstallGrants) *[]string { return &g.Read }}, func(r plugins.InstallRequests) bool { return r.Read }},
	{installKind{"send", "send messages to it", func(g *pluginfeed.PluginInstallGrants) *[]string { return &g.Send }}, func(r plugins.InstallRequests) bool { return r.Send }},
	{installKind{"accept", "it may message this plugin", func(g *pluginfeed.PluginInstallGrants) *[]string { return &g.InboxFrom }}, func(r plugins.InstallRequests) bool { return r.AcceptFrom }},
}

// screenStyle is how the session rows are drawn.
type screenStyle int

const (
	screenKeys  screenStyle = iota // checkbox screen with a cursor
	screenPlan                     // the --yes plan: boxes show what was accepted
	screenLines                    // line mode: numbered rows, the questions follow
)

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

// sessionLabel names a session for the owner; an unnamed one says so.
func sessionLabel(session model.Session) string {
	if session.Name == "" {
		return "(no name)"
	}
	return plainText(session.Name)
}

// installScreen is the single review screen. Nothing starts selected.
type installScreen struct {
	header    string
	kinds     []installKind
	inboxRead bool // the manifest asked for inbox_read
	sessions  []model.Session
	selected  map[[2]int]bool
	inboxOn   bool
	row, col  int
	notice    string
}

func newInstallScreen(header string, requests plugins.InstallRequests, sessions []model.Session) *installScreen {
	s := &installScreen{header: header, inboxRead: requests.InboxRead, sessions: sessions, selected: map[[2]int]bool{}}
	for _, kind := range installKinds {
		if kind.requested(requests) {
			s.kinds = append(s.kinds, kind.installKind)
		}
	}
	return s
}

func (s *installScreen) rows() int {
	if s.inboxRead {
		return len(s.sessions) + 1
	}
	return len(s.sessions)
}

func (s *installScreen) grants() pluginfeed.PluginInstallGrants {
	var g pluginfeed.PluginInstallGrants
	for key, on := range s.selected {
		if on {
			field := s.kinds[key[1]].field(&g)
			*field = append(*field, s.sessions[key[0]].ID)
		}
	}
	g.InboxRead = s.inboxOn
	return g
}

// check returns a plain reason when the current choices cannot be installed.
func (s *installScreen) check() string {
	g := s.grants()
	if len(g.Sessions) == 0 {
		return "Select at least one session for changes."
	}
	ids := slices.Concat(g.Read, g.Send, g.InboxFrom)
	slices.Sort(ids)
	if len(g.Sessions) > 16 || len(slices.Compact(ids)) > 16 {
		return "Select at most 16 sessions per kind of access."
	}
	return ""
}

func (s *installScreen) render(style screenStyle) string {
	var b strings.Builder
	b.WriteString(s.header)
	legend := make([]string, 0, len(s.kinds))
	for _, kind := range s.kinds {
		legend = append(legend, kind.label+" = "+kind.help)
	}
	b.WriteString("Access per session: " + strings.Join(legend, "; ") + "\n")
	cursor := style == screenKeys
	if cursor {
		b.WriteString("Arrows move, Space changes a choice, y installs, n or Enter cancels.\n")
	}
	for i, session := range s.sessions {
		if style != screenLines {
			b.WriteString(s.mark(cursor && s.row == i))
			for j, kind := range s.kinds {
				on := s.selected[[2]int{i, j}]
				b.WriteString(s.box(cursor && s.row == i && s.col == j, on) + kind.label + "  ")
			}
		}
		fmt.Fprintf(&b, "%d. %s  %s\n", i+1, sessionLabel(session), plainText(session.ID))
	}
	if s.inboxRead && style != screenLines {
		b.WriteString(s.mark(cursor && s.row == len(s.sessions)) + s.box(cursor && s.row == len(s.sessions), s.inboxOn) + "read messages stored in this plugin's own inbox\n")
	}
	if s.notice != "" {
		b.WriteString(s.notice + "\n")
	}
	b.WriteString("Install and enable with these choices? [y/N] ")
	return b.String()
}

func (s *installScreen) mark(here bool) string {
	if here {
		return "> "
	}
	return "  "
}

func (s *installScreen) box(focus, on bool) string {
	text := "[ ]"
	if on {
		text = "[x]"
	}
	if focus {
		return "\x1b[7m" + text + "\x1b[0m "
	}
	return text + " "
}

// key applies one key. It returns done and whether the owner said yes.
func (s *installScreen) key(k string) (bool, bool) {
	s.notice = ""
	switch k {
	case "up", "k":
		s.row = max(s.row-1, 0)
	case "down", "j":
		s.row = min(s.row+1, s.rows()-1)
	case "left", "h":
		s.col = max(s.col-1, 0)
	case "right", "l":
		s.col = min(s.col+1, len(s.kinds)-1)
	case " ":
		if s.row == len(s.sessions) {
			s.inboxOn = !s.inboxOn
		} else if s.row < len(s.sessions) {
			key := [2]int{s.row, s.col}
			s.selected[key] = !s.selected[key]
		}
	case "y", "Y":
		if s.notice = s.check(); s.notice == "" {
			return true, true
		}
	case "n", "N", "q", "enter", "ctrl-c":
		return true, false
	}
	return false, false
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

// chooseRaw runs the checkbox screen on raw key input.
func (s *installScreen) chooseRaw(in io.Reader, out io.Writer) (bool, error) {
	keys := bufio.NewReader(in)
	for {
		if _, err := io.WriteString(out, "\x1b[H\x1b[2J"+strings.ReplaceAll(s.render(screenKeys), "\n", "\r\n")); err != nil {
			return false, err
		}
		k := readInstallKey(keys)
		if k == "eof" { // Closed input means no, and the screen is never redrawn again.
			_, err := io.WriteString(out, "\r\n")
			return false, err
		}
		if done, yes := s.key(k); done {
			_, err := io.WriteString(out, "\r\n")
			return yes, err
		}
	}
}

// chooseLines is the plain fallback when raw keys are unavailable.
func (s *installScreen) chooseLines(in io.Reader, out io.Writer) (bool, error) {
	lines := bufio.NewReader(in)
	if _, err := io.WriteString(out, strings.TrimSuffix(s.render(screenLines), "Install and enable with these choices? [y/N] ")); err != nil {
		return false, err
	}
	for j, kind := range s.kinds {
		if err := s.askNumbers(lines, out, j, kind.label); err != nil {
			return false, err
		}
	}
	if s.inboxRead {
		answer, err := askLine(lines, out, "Allow reading this plugin's own inbox? [y/N] ")
		if err != nil {
			return false, err
		}
		s.inboxOn = answer == "y" || answer == "yes"
	}
	if s.notice = s.check(); s.notice != "" {
		_, err := io.WriteString(out, s.notice+" Nothing was installed.\n")
		return false, err
	}
	answer, err := askLine(lines, out, "Install and enable with these choices? [y/N] ")
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

func (s *installScreen) askNumbers(lines *bufio.Reader, out io.Writer, col int, label string) error {
	for range 3 {
		answer, err := askLine(lines, out, "Sessions for "+label+" (enter session numbers, e.g. 1,3; empty for none): ")
		if err != nil {
			return err
		}
		if picked, ok := s.parseNumbers(answer); ok {
			for _, row := range picked {
				s.selected[[2]int{row, col}] = true
			}
			return nil
		}
		if _, err := io.WriteString(out, s.numberHelp()); err != nil {
			return err
		}
	}
	return fmt.Errorf("no valid session numbers for %s; nothing was installed", label)
}

func (s *installScreen) numberHelp() string {
	if len(s.sessions) == 1 {
		return "Use a number from the list (1).\n"
	}
	return fmt.Sprintf("Use numbers from the list (1-%d), separated by commas.\n", len(s.sessions))
}

func (s *installScreen) parseNumbers(answer string) ([]int, bool) {
	var rows []int
	for part := range strings.SplitSeq(answer, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > len(s.sessions) {
			return nil, false
		}
		rows = append(rows, n-1)
	}
	return rows, true
}
