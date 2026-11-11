package main

import (
	"bufio"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/plugins"
)

// installKind is one kind of access the manifest asked for, per session.
type installKind struct {
	label string
	field func(*bus.PluginInstallGrants) *[]string
}

var installKinds = []struct {
	installKind
	requested func(plugins.InstallRequests) bool
}{
	{installKind{"changes", func(g *bus.PluginInstallGrants) *[]string { return &g.Sessions }}, func(r plugins.InstallRequests) bool { return r.Feed }},
	{installKind{"read", func(g *bus.PluginInstallGrants) *[]string { return &g.Read }}, func(r plugins.InstallRequests) bool { return r.Read }},
	{installKind{"send", func(g *bus.PluginInstallGrants) *[]string { return &g.Send }}, func(r plugins.InstallRequests) bool { return r.Send }},
	{installKind{"accept", func(g *bus.PluginInstallGrants) *[]string { return &g.InboxFrom }}, func(r plugins.InstallRequests) bool { return r.AcceptFrom }},
}

const installKindHelp = "changes = see message changes from that session; read = read message bodies; send = send to it; accept = it may message this plugin"

// plainText shows owner-facing text safely: anything not printable is quoted.
func plainText(value string) string {
	if strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsPrint(r) }) {
		return strconv.Quote(value)
	}
	return value
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

func (s *installScreen) grants() bus.PluginInstallGrants {
	var g bus.PluginInstallGrants
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

func (s *installScreen) render(cursor bool) string {
	var b strings.Builder
	b.WriteString(s.header)
	b.WriteString("Access (" + installKindHelp + "):\n")
	if cursor {
		b.WriteString("Arrows move, Space changes a choice, y installs, n or Enter cancels.\n")
	}
	for i, session := range s.sessions {
		b.WriteString(s.mark(cursor && s.row == i))
		for j, kind := range s.kinds {
			on := s.selected[[2]int{i, j}]
			b.WriteString(s.box(cursor && s.row == i && s.col == j, on) + kind.label + "  ")
		}
		fmt.Fprintf(&b, "%d. %s  %s\n", i+1, plainText(session.Name), plainText(session.ID))
	}
	if s.inboxRead {
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
	if next, err := r.ReadByte(); err != nil || (next != '[' && next != 'O') {
		return "esc"
	}
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
		if _, err := io.WriteString(out, "\x1b[H\x1b[2J"+strings.ReplaceAll(s.render(true), "\n", "\r\n")); err != nil {
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
	if _, err := io.WriteString(out, strings.TrimSuffix(s.render(false), "Install and enable with these choices? [y/N] ")); err != nil {
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
		if _, err := fmt.Fprintf(out, "Use numbers from 1 to %d, separated by commas.\n", len(s.sessions)); err != nil {
			return err
		}
	}
	return fmt.Errorf("no valid session numbers for %s; nothing was installed", label)
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
