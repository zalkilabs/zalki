package main

import (
	"errors"
	"sort"
	"strconv"
	"strings"
)

var (
	errPluginReviewCandidates = errors.New("too many or invalid plugin install candidates")
	errPluginReviewTarget     = errors.New("choose #row, @actorID or a unique simple name from this snapshot")
	errPluginReviewAmbiguous  = errors.New("that name or list is unclear; use #row or @actorID")
	errPluginReviewTargetCap  = errors.New("choose at most 16 sessions for this access")
)

// Caller supplies validated native candidates and revalidates chosen IDs at
// commit. Parse against this exact snapshot; never grant by current name later.
func pickPluginReviewTargets(input string, candidates []pluginReviewTarget) ([]string, error) {
	if len(input) > 4096 {
		return nil, errPluginReviewCandidates
	}
	index, err := indexPluginReviewCandidates(candidates)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(input) == "" {
		return []string{}, nil
	}
	tokens := strings.Split(input, ",")
	for i := range tokens {
		tokens[i] = strings.TrimSpace(tokens[i])
		if tokens[i] == "" {
			return nil, errPluginReviewTarget
		}
	}
	if reviewCommaShadow(tokens, index) {
		return nil, errPluginReviewAmbiguous
	}
	selected := make(map[string]bool)
	for _, token := range tokens {
		id, err := resolvePluginReviewTarget(token, index)
		if err != nil {
			return nil, err
		}
		selected[id] = true
		if len(selected) > 16 {
			return nil, errPluginReviewTargetCap
		}
	}
	ids := make([]string, 0, len(selected))
	for id := range selected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func resolvePluginReviewTarget(token string, index pluginReviewCandidates) (string, error) {
	if number, ok := strings.CutPrefix(token, "#"); ok {
		row, err := strconv.Atoi(number)
		if err == nil && strconv.Itoa(row) == number && row >= 1 && row <= len(index.rows) {
			return index.rows[row-1].target.id, nil
		}
		return "", errPluginReviewTarget
	}
	if id, ok := strings.CutPrefix(token, "@"); ok {
		if entry, exists := index.byID[id]; exists {
			return entry.target.id, nil
		}
		return "", errPluginReviewTarget
	}
	if !simplePluginReviewName(token) {
		return "", errPluginReviewTarget
	}
	for _, entry := range index.rows {
		if entry.target.name == token {
			if !entry.bareAllowed {
				return "", errPluginReviewAmbiguous
			}
			return entry.target.id, nil
		}
	}
	return "", errPluginReviewTarget
}
