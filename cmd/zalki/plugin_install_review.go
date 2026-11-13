package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

const pluginReviewQuestion = "Install it? [y/N]"

var errPluginReview = errors.New("invalid plugin review plan")

type pluginReviewAccess struct {
	status, receive, send, read bool
}

type pluginReviewTarget struct {
	id, name string
}

// pluginReviewPlan is a presentation snapshot, not permission to build or commit.
// The caller must supply W-validated manifest values and reviewed target IDs.
// Backend authentication, membership, pinning and commit checks remain mandatory.
type pluginReviewPlan struct {
	name, id, version, source, home string
	build, run                      []string
	access                          pluginReviewAccess
	send, read                      []string
	candidates                      []pluginReviewTarget
}

// renderPluginReview has no terminal, process, journal or environment access.
// It renders slice 4 only; future supervisor consent is not wired here.
func renderPluginReview(p pluginReviewPlan) ([]string, error) {
	if err := validatePluginReview(p); err != nil {
		return nil, err
	}
	lines := []string{
		fmt.Sprintf("Install %q (%s %s, author's claim) from %s", p.name, reviewText(p.id), reviewText(p.version), reviewSource(p.source, p.home)),
		"It runs as you, with no sandbox. It can do anything your user can do.",
		reviewBuildLine(p),
		"In Zalki it will:",
	}
	accessLines, err := reviewAccessLines(p)
	if err != nil {
		return nil, err
	}
	lines = append(lines, accessLines...)
	if p.access.send && !p.access.read {
		lines = append(lines, "It can send only to the sessions named above. Change later: zalki plugins grant "+reviewText(p.name))
	}
	lines = append(lines, pluginReviewQuestion)
	if len(lines) > 9 {
		return nil, errPluginReview
	}
	return lines, nil
}

func reviewAccessLines(p pluginReviewPlan) ([]string, error) {
	var lines []string
	if p.access.status {
		lines = append(lines, "  see the status of all sessions, now and later")
	}
	if p.access.receive {
		lines = append(lines, "  get messages that sessions send to it, and answer the sender")
	}
	if p.access.send {
		labels, err := reviewTargetLabels(p.send, p.candidates)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "  SEND messages on its own to: "+labels)
	}
	if p.access.read {
		labels, err := reviewTargetLabels(p.read, p.candidates)
		if err != nil {
			return nil, err
		}
		lines = append(lines, "  READ the messages of: "+labels)
	}
	return lines, nil
}

func reviewBuildLine(p pluginReviewPlan) string {
	if len(p.build) == 0 {
		return "Ready-made artifact; runs: " + reviewArgv(p.run)
	}
	return "Built here with: " + reviewArgv(p.build) + "  Runs: " + reviewArgv(p.run)
}

func reviewArgv(args []string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = strconv.QuoteToASCII(arg)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func reviewSource(source, home string) string {
	if home != "" && home != string(filepath.Separator) {
		if source == home {
			source = "~"
		} else if suffix, ok := strings.CutPrefix(source, home+string(filepath.Separator)); ok {
			source = "~/" + suffix
		}
	}
	return reviewText(source)
}

func reviewText(value string) string {
	if strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsPrint(r) || r == '\u2028' || r == '\u2029' }) {
		return strconv.QuoteToASCII(value)
	}
	return value
}

// Use selected IDs, never caller-supplied selected labels. Full-snapshot
// ambiguity remains visible even when just one of two duplicates is selected.
func reviewTargetLabels(ids []string, candidates []pluginReviewTarget) (string, error) {
	index, err := indexPluginReviewCandidates(candidates)
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		return "nobody", nil
	}
	ordered := append([]string(nil), ids...)
	sort.Strings(ordered)
	labels := make([]string, 0, min(3, len(ordered)))
	for i, id := range ordered {
		entry, exists := index.byID[id]
		if !exists || i > 0 && ordered[i-1] == id {
			return "", errPluginReview
		}
		if i >= 3 {
			continue
		}
		label := entry.label
		if !entry.bareAllowed {
			label += " (@" + reviewText(id) + ")"
		}
		labels = append(labels, label)
	}
	text := strings.Join(labels, ", ")
	if len(ordered) > 3 {
		text += fmt.Sprintf(" and %d more (see --details)", len(ordered)-3)
	}
	return text, nil
}

func validatePluginReview(p pluginReviewPlan) error {
	if p.name == "" || p.id == "" || p.version == "" || p.source == "" || len(p.run) == 0 {
		return errPluginReview
	}
	return validatePluginReviewAccess(p)
}

func validatePluginReviewAccess(p pluginReviewPlan) error {
	if !p.access.status && !p.access.receive && !p.access.send && !p.access.read {
		return errPluginReview
	}
	if (!p.access.send && len(p.send) != 0) || (!p.access.read && len(p.read) != 0) || len(p.send) > 16 || len(p.read) > 16 {
		return errPluginReview
	}
	return nil
}
