package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

func (p *pluginInstaller) review() (pluginfeed.PluginAccess, bool, error) {
	p.terminalReviewed = false
	m := p.source.Manifest
	access := pluginfeed.PluginAccess{Status: m.Access.Status, Receive: m.Access.Receive}
	if p.flags.yes {
		return access, true, p.reviewFlags()
	}
	choice, restore := installTerminal(p.cmd.InOrStdin())
	defer func() { restore() }()
	if choice == installNoTerminal {
		return access, false, pluginReviewError("Run this in your own terminal, or use --yes with --tree-sha256 and --allow-unsandboxed for standard access.")
	}
	input := newContextReader(p.cmd.Context(), p.cmd.InOrStdin())
	input.maxRead = 1
	keys := bufio.NewReader(input)
	plan := p.reviewPlan(access, nil)
	if m.Access.Send || m.Access.Read {
		restore()
		restore = func() {}
		var ok bool
		var err error
		access, plan, ok, err = p.selectReviewAccess(keys, access)
		if err != nil || !ok {
			return access, false, p.reviewReadError(err)
		}
		choice, restore = installTerminal(p.cmd.InOrStdin())
		if choice == installNoTerminal {
			return access, false, errors.New("the review terminal is no longer available")
		}
	}
	lines, err := renderPluginReview(plan)
	if err != nil {
		return access, false, err
	}
	if err := p.writeReview(lines[:len(lines)-1]); err != nil {
		return access, false, err
	}
	if p.flags.details {
		if err := p.say("Exact SEND targets: %s\nExact READ targets: %s\n", pluginExactTargets(access.Send), pluginExactTargets(access.Read)); err != nil {
			return access, false, err
		}
	}
	yes, err := p.confirmReview(keys, choice == installRawKeys, access)
	p.terminalReviewed = yes && err == nil
	return access, yes, err
}

func (p *pluginInstaller) reviewPlan(access pluginfeed.PluginAccess, targets []pluginReviewTarget) pluginReviewPlan {
	m := p.source.Manifest
	return pluginReviewPlan{
		name: p.name, id: m.ID, version: m.Version, source: p.folder, home: p.home,
		build: m.Build, run: append([]string{m.Artifact}, m.Args...),
		access: pluginReviewAccess{m.Access.Status, m.Access.Receive, m.Access.Send, m.Access.Read},
		send:   access.Send, read: access.Read, candidates: targets,
	}
}

func (p *pluginInstaller) selectReviewAccess(keys *bufio.Reader, access pluginfeed.PluginAccess) (pluginfeed.PluginAccess, pluginReviewPlan, bool, error) {
	targets, err := p.reviewTargets()
	if err != nil {
		return access, pluginReviewPlan{}, false, err
	}
	for _, kind := range []struct {
		name      string
		requested bool
		ids       *[]string
	}{{"SEND", p.source.Manifest.Access.Send, &access.Send}, {"READ", p.source.Manifest.Access.Read, &access.Read}} {
		if !kind.requested {
			continue
		}
		ids, ok, err := p.pickReviewKind(keys, kind.name, targets)
		if err != nil || !ok {
			return access, pluginReviewPlan{}, false, err
		}
		*kind.ids = ids
	}
	return access, p.reviewPlan(access, targets), true, nil
}

func (p *pluginInstaller) reviewTargets() ([]pluginReviewTarget, error) {
	sessions, skipped, err := p.store.ListWithWarnings()
	if err != nil || skipped != 0 {
		return nil, errors.New("could not safely list running sessions; run zalki doctor")
	}
	var targets []pluginReviewTarget
	for _, session := range sessions {
		if session.Retired || session.Liveness == model.LivenessExited || session.Kind == "plugin" {
			continue
		}
		if !model.ValidKind(session.Kind) || model.ValidateSessionAddress(session.ID) != nil || !strings.HasPrefix(session.ID, session.Kind+":") {
			return nil, errPluginReviewCandidates
		}
		targets = append(targets, pluginReviewTarget{session.ID, session.Name})
		if len(targets) > 64 {
			return nil, errPluginReviewCandidates
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].id < targets[j].id })
	return targets, nil
}

func (p *pluginInstaller) pickReviewKind(keys *bufio.Reader, kind string, targets []pluginReviewTarget) ([]string, bool, error) {
	rows, err := renderPluginReviewCandidates(targets)
	if err != nil {
		return nil, false, err
	}
	if err := p.say("%s requests %s access. Select named running sessions.\n%s\n", p.name, kind, strings.Join(rows, "\n")); err != nil {
		return nil, false, err
	}
	for range 3 {
		if err := p.say("%s targets (#row, @actorID or a unique simple name; blank = nobody): ", kind); err != nil {
			return nil, false, err
		}
		line, complete, err := readPluginReviewLine(keys, 4096)
		if err != nil || !complete {
			return nil, false, err
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

func (p *pluginInstaller) writeReview(lines []string) error {
	if p.flags.details {
		if err := p.say("%s", p.header()); err != nil {
			return err
		}
	}
	return p.say("%s\n", strings.Join(lines, "\n"))
}

func (p *pluginInstaller) confirmReview(keys *bufio.Reader, raw bool, access pluginfeed.PluginAccess) (bool, error) {
	for range 3 {
		if err := p.say("%s", installQuestion); err != nil {
			return false, err
		}
		answer, err := readPluginReviewAnswer(keys, raw)
		if err != nil {
			return false, p.reviewReadError(err)
		}
		if err := p.say("\n"); err != nil {
			return false, err
		}
		switch answer {
		case pluginReviewAccept:
			return true, p.warnEmptyExtended(access)
		case pluginReviewCancel:
			return false, nil
		case pluginReviewInvalid:
			continue
		}
	}
	return false, nil
}

func (p *pluginInstaller) reviewReadError(err error) error {
	if p.cmd.Context().Err() != nil || cancelled(err) {
		return errInstallCancelled
	}
	return err
}

func (p *pluginInstaller) reviewFlags() error {
	switch {
	case !validReviewHash(p.flags.tree):
		return pluginReviewError("With --yes, provide --tree-sha256 with the folder's SHA256.")
	case !p.flags.allowUnsandboxed:
		return pluginReviewError("With --yes, add --allow-unsandboxed to accept running native code as you.")
	case p.flags.tree != p.source.TreeSHA256:
		return pluginReviewError("The source changed: --tree-sha256 does not match the copied folder. Nothing was installed.")
	}
	lines, err := renderPluginReview(p.reviewPlan(pluginfeed.PluginAccess{}, nil))
	if err != nil {
		return err
	}
	lines[len(lines)-1] = "Accepted by --yes (standard access only)."
	return p.writeReview(lines)
}

func validReviewHash(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, c := range value {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' {
			continue
		}
		return false
	}
	return true
}

func (p *pluginInstaller) extendedYesError(access plugins.InstallAccess) error {
	var kinds []string
	if access.Send {
		kinds = append(kinds, "SEND")
	}
	if access.Read {
		kinds = append(kinds, "READ")
	}
	command := "zalki plugins install " + pluginCommandWord(p.folder)
	if p.flags.as != "" {
		command += " --as " + pluginCommandWord(p.name)
	}
	return fmt.Errorf("%s asks for extended access (%s). Run this in your own terminal: %s", p.name, strings.Join(kinds, ", "), command)
}

func pluginCommandWord(value string) string {
	if value != "" && !strings.ContainsFunc(value, func(r rune) bool {
		allowed := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+@:,=", r)
		return !allowed
	}) {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func printablePluginPath(value string) bool {
	return utf8.ValidString(value) && !strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsPrint(r) })
}

func (p *pluginInstaller) warnEmptyExtended(access pluginfeed.PluginAccess) error {
	for _, kind := range []struct {
		name      string
		requested bool
		count     int
	}{
		{"SEND", p.source.Manifest.Access.Send, len(access.Send)}, {"READ", p.source.Manifest.Access.Read, len(access.Read)},
	} {
		if kind.requested && kind.count == 0 {
			if _, err := fmt.Fprintf(p.cmd.ErrOrStderr(), "Warning: %s has no %s targets.\n", p.name, kind.name); err != nil {
				return errors.New("could not write the plugin access warning")
			}
		}
	}
	return nil
}

func (p *pluginInstaller) reportInstalled() error {
	status, err := pluginfeed.NewPluginFeed(p.store, p.name).PluginStatus()
	if err != nil {
		return fmt.Errorf("installed %s, but its committed details could not be read; run zalki plugins status %s", p.name, p.name)
	}
	if status.Install != nil && status.Install.InstalledBy == "unproven" {
		if _, err := io.WriteString(p.cmd.ErrOrStderr(), "Installer could not be verified; recorded as unproven.\n"); err != nil {
			return errors.New("installed, but the installer warning could not be written")
		}
	}
	return p.say("Installed. Start it with: zalki plugins run %s\n", p.name)
}

// Product-facing consent failures preserve the reviewed sentence verbatim.
func pluginReviewError(message string) error { return errors.New(message) }
