package main

import (
	"fmt"
	"strconv"
	"strings"
)

const pluginReviewLabelLimit = 80

type pluginReviewCandidate struct {
	target      pluginReviewTarget
	row         int
	label       string
	bareAllowed bool
}

type pluginReviewCandidates struct {
	rows []pluginReviewCandidate
	byID map[string]pluginReviewCandidate
}

// Index the ENTIRE frozen snapshot. Never compute label uniqueness using just
// the chosen subset. This presentation index grants no authority to its labels.
func indexPluginReviewCandidates(targets []pluginReviewTarget) (pluginReviewCandidates, error) {
	if len(targets) > 64 {
		return pluginReviewCandidates{}, errPluginReviewCandidates
	}
	index := pluginReviewCandidates{byID: make(map[string]pluginReviewCandidate, len(targets))}
	trimCounts, labelCounts := map[string]int{}, map[string]int{}
	for i, target := range targets {
		if target.id == "" {
			return pluginReviewCandidates{}, errPluginReviewCandidates
		}
		if _, exists := index.byID[target.id]; exists {
			return pluginReviewCandidates{}, errPluginReviewCandidates
		}
		label, shortened := quotePluginReviewLabel(target.name)
		entry := pluginReviewCandidate{target: target, row: i + 1, label: label, bareAllowed: simplePluginReviewName(target.name) && !shortened}
		index.rows = append(index.rows, entry)
		index.byID[target.id] = entry
		trimCounts[strings.TrimSpace(target.name)]++
		labelCounts[label]++
	}
	for i := range index.rows {
		entry := &index.rows[i]
		entry.bareAllowed = entry.bareAllowed && trimCounts[entry.target.name] == 1 && labelCounts[entry.label] == 1
		index.byID[entry.target.id] = *entry
	}
	return index, nil
}

func simplePluginReviewName(name string) bool {
	if len(name) == 0 || len(name) > 64 || strings.EqualFold(name, "all") {
		return false
	}
	for i, c := range []byte(name) {
		letter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
		if letter || i > 0 && (c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			continue
		}
		return false
	}
	return true
}

// Quote and cap display text without cutting an escape sequence. A shortened
// or non-simple name is ALWAYS row/ID-only, even without a visible collision.
func quotePluginReviewLabel(name string) (string, bool) {
	var out strings.Builder
	out.WriteByte('"')
	shortened := false
	for _, r := range name {
		quoted := strconv.QuoteToASCII(string(r))
		part := quoted[1 : len(quoted)-1]
		if out.Len()+len(part)+4 > pluginReviewLabelLimit {
			shortened = true
			break
		}
		out.WriteString(part)
	}
	if shortened {
		out.WriteString("...")
	}
	out.WriteByte('"')
	return out.String(), shortened
}

// Rows always carry explicit identity, including duplicate/unsafe labels.
func renderPluginReviewCandidates(targets []pluginReviewTarget) ([]string, error) {
	index, err := indexPluginReviewCandidates(targets)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(index.rows))
	for _, entry := range index.rows {
		lines = append(lines, fmt.Sprintf("#%d %s (@%s)", entry.row, entry.label, reviewText(entry.target.id)))
	}
	return lines, nil
}

// A comma-bearing label can imitate a LIST of otherwise valid bare names.
// Refuse any such contiguous bare-name expression; explicit #/@ tokens retain
// their reserved syntax and cannot be reinterpreted as a display name.
func reviewCommaShadow(tokens []string, index pluginReviewCandidates) bool {
	for _, entry := range index.rows {
		if !strings.Contains(entry.target.name, ",") {
			continue
		}
		parts := strings.Split(entry.target.name, ",")
		for i := range parts {
			parts[i] = strings.TrimSpace(parts[i])
		}
		if reviewContainsBareSequence(tokens, parts) {
			return true
		}
	}
	return false
}

func reviewContainsBareSequence(tokens, parts []string) bool {
	for start := 0; start+len(parts) <= len(tokens); start++ {
		match := true
		for i, part := range parts {
			if !simplePluginReviewName(part) || tokens[start+i] != part {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
