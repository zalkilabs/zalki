package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

const updateQuestion = "Update with these choices? [y/N; s=toggle STATUS, r=toggle RECEIVE] "

func (p *pluginInstaller) retainedUpdateAccess() pluginfeed.PluginAccess {
	a := p.update.Access
	m := p.source.Manifest.Access
	var old plugins.InstallAccess
	// Receipts have already been strictly validated by the package-owned read.
	_ = json.Unmarshal(p.update.Install.Access, &old)
	a.Status, a.Receive = a.Status && m.Status && old.Status, a.Receive && m.Receive && old.Receive
	a.Send, a.Read = slices.Clone(a.Send), slices.Clone(a.Read)
	if !m.Send || !old.Send {
		a.Send = nil
	}
	if !m.Read || !old.Read {
		a.Read = nil
	}
	return a
}

func (p *pluginInstaller) reviewUpdate() (pluginfeed.PluginAccess, bool, error) {
	p.terminalReviewed = false
	a := p.retainedUpdateAccess()
	m := p.source.Manifest.Access
	if p.flags.yes {
		return p.reviewUpdateYes(a)
	}
	choice, restore := installTerminal(p.cmd.InOrStdin())
	defer func() { restore() }()
	if choice == installNoTerminal {
		return a, false, errors.New("review this update in your own terminal; --yes permits only unchanged standard access")
	}
	input := newContextReader(p.cmd.Context(), p.cmd.InOrStdin())
	input.maxRead = 1
	keys := bufio.NewReader(input)
	var targets []pluginReviewTarget
	if m.Send || m.Read {
		var err error
		targets, err = p.reviewTargets()
		if err != nil {
			return a, false, err
		}
		restore()
		restore = func() {}
		for _, kind := range []struct {
			name      string
			requested bool
			ids       *[]string
		}{{"SEND", m.Send, &a.Send}, {"READ", m.Read, &a.Read}} {
			if !kind.requested {
				continue
			}
			ids, ok, err := p.pickUpdateKind(keys, kind.name, targets, *kind.ids)
			if err != nil || !ok {
				return a, false, p.reviewReadError(err)
			}
			*kind.ids = ids
		}
		choice, restore = installTerminal(p.cmd.InOrStdin())
		if choice == installNoTerminal {
			return a, false, errors.New("the review terminal is no longer available")
		}
	}
	return p.confirmUpdate(keys, choice == installRawKeys, a, targets)
}

func (p *pluginInstaller) reviewUpdateYes(a pluginfeed.PluginAccess) (pluginfeed.PluginAccess, bool, error) {
	m := p.source.Manifest.Access
	var old plugins.InstallAccess
	if err := json.Unmarshal(p.update.Install.Access, &old); err != nil {
		return a, false, err
	}
	if m != old || m.Send || m.Read || len(p.update.Access.Send)+len(p.update.Access.Read) != 0 || a.Status != p.update.Access.Status || a.Receive != p.update.Access.Receive {
		return a, false, errors.New("--yes requires unchanged declared and granted standard access; run this update in your own terminal")
	}
	if !validReviewHash(p.flags.tree) || p.flags.tree != p.source.TreeSHA256 || !p.flags.allowUnsandboxed {
		return a, false, errors.New("--yes needs the matching --tree-sha256 and --allow-unsandboxed")
	}
	lines, err := p.updateReviewLines(a, nil)
	if err == nil {
		lines[len(lines)-1] = "Accepted by --yes (unchanged standard access)."
		err = p.writeReview(lines)
	}
	return a, err == nil, err
}

func (p *pluginInstaller) confirmUpdate(keys *bufio.Reader, raw bool, a pluginfeed.PluginAccess, targets []pluginReviewTarget) (pluginfeed.PluginAccess, bool, error) {
	m := p.source.Manifest.Access
	for range 16 {
		lines, err := p.updateReviewLines(a, targets)
		if err != nil {
			return a, false, err
		}
		if err := p.writeReview(lines); err != nil {
			return a, false, err
		}
		answer, err := readUpdateAnswer(keys, raw)
		if err != nil {
			return a, false, p.reviewReadError(err)
		}
		switch answer {
		case "y", "yes":
			p.terminalReviewed = true
			return a, true, p.warnEmptyExtended(a)
		case "s":
			if m.Status {
				a.Status = !a.Status
			}
		case "r":
			if m.Receive {
				a.Receive = !a.Receive
			}
		case "", "n", "no", "q", "c", "cancel":
			return a, false, nil
		}
	}
	return a, false, nil
}

func readUpdateAnswer(keys *bufio.Reader, raw bool) (string, error) {
	if raw {
		key, err := keys.Peek(1)
		if err == nil && (key[0] == 's' || key[0] == 'S' || key[0] == 'r' || key[0] == 'R') {
			_, err = keys.ReadByte()
			return strings.ToLower(string(key)), err
		}
		answer, err := readPluginReviewAnswer(keys, true)
		if answer == pluginReviewAccept {
			return "y", err
		}
		if answer == pluginReviewCancel {
			return "", err
		}
		return "invalid", err
	}
	line, complete, err := readPluginReviewLine(keys, 256)
	if !complete {
		return "", err
	}
	return strings.ToLower(strings.Trim(line, " \t\r")), err
}

func (p *pluginInstaller) pickUpdateKind(keys *bufio.Reader, kind string, targets []pluginReviewTarget, kept []string) ([]string, bool, error) {
	var current []string
	for _, id := range kept {
		if slices.ContainsFunc(targets, func(target pluginReviewTarget) bool { return target.id == id }) {
			current = append(current, id)
		}
	}
	rows, err := renderPluginReviewCandidates(targets)
	if err != nil {
		return nil, false, err
	}
	if err := p.say("%s targets (blank keeps %s; - removes all; select #row, @actorID or unique name):\n%s\n", kind, pluginExactTargets(current), strings.Join(rows, "\n")); err != nil {
		return nil, false, err
	}
	for range 3 {
		line, complete, err := readPluginReviewLine(keys, 4096)
		if err != nil || !complete {
			return nil, false, err
		}
		if strings.TrimSpace(line) == "" {
			return current, true, nil
		}
		if strings.TrimSpace(line) == "-" {
			return nil, true, nil
		}
		ids, err := pickPluginReviewTargets(line, targets)
		if err == nil {
			return ids, true, nil
		}
		if err := p.say("%s\n", err.Error()); err != nil {
			return nil, false, err
		}
	}
	return nil, false, nil
}

func (p *pluginInstaller) updateReviewLines(a pluginfeed.PluginAccess, targets []pluginReviewTarget) ([]string, error) {
	var old plugins.InstallAccess
	if err := json.Unmarshal(p.update.Install.Access, &old); err != nil {
		return nil, err
	}
	m := p.source.Manifest
	lines := []string{fmt.Sprintf("Update %q (%s %s, author's claim); keeps identity/data; stays %s", p.name, reviewText(m.ID), reviewText(m.Version), enabledText(p.update.Enabled))}
	for _, kind := range []updateAccessDiff{
		{"STATUS", p.update.Access.Status, a.Status, m.Access.Status, old.Status, nil, nil},
		{"RECEIVE", p.update.Access.Receive, a.Receive, m.Access.Receive, old.Receive, nil, nil},
		{"SEND", false, false, m.Access.Send, old.Send, p.update.Access.Send, a.Send},
		{"READ", false, false, m.Access.Read, old.Read, p.update.Access.Read, a.Read},
	} {
		line, err := kind.line(targets, p.update.Scope)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	source := reviewSource(p.folder, p.home)
	if p.folder != p.update.Install.Source {
		source = reviewSource(p.update.Install.Source, p.home) + " -> " + source + " (moved)"
	}
	build := reviewBuildLine(p.reviewPlan(a, targets))
	if !slices.Equal(m.Build, p.update.Install.Build) {
		build += " Was: " + reviewArgv(p.update.Install.Build)
	}
	lines = append(lines,
		fmt.Sprintf("Source: %s; snapshot %s -> %s; pin %s -> checked after build", source, p.update.Install.TreeSHA256[:12], p.source.TreeSHA256[:12], p.update.Install.ArtifactSHA256[:12]),
		"It runs as you, with no sandbox. It can do anything your user can do.",
		build,
		updateQuestion,
	)
	return lines, nil
}

type updateAccessDiff struct {
	name                            string
	was, now, requested, previously bool
	before, after                   []string
}

func (kind updateAccessDiff) line(targets []pluginReviewTarget, scope string) (string, error) {
	before, after := strconv.FormatBool(kind.was), strconv.FormatBool(kind.now)
	if kind.name == "SEND" || kind.name == "READ" {
		before = pluginExactTargets(kind.before)
		var err error
		after, err = reviewTargetLabels(kind.after, targets)
		if err != nil {
			return "", err
		}
	} else {
		after += " (scope " + pluginScopeText(scope) + ")"
	}
	label := ""
	switch {
	case !kind.requested && (kind.previously || kind.was || len(kind.before) > 0):
		label = " (removed)"
	case kind.requested && !kind.previously:
		label = " (NEW request; starts unselected)"
	case slices.ContainsFunc(kind.before, func(id string) bool { return !slices.Contains(kind.after, id) }):
		label = " (removed targets)"
	}
	return kind.name + ": " + before + " -> " + after + label, nil
}
