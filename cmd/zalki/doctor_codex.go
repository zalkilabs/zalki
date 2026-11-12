package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/bus"
)

type codexHooksDiagnostic struct {
	Status      string                `json:"status"`
	Message     string                `json:"message"`
	Advice      string                `json:"advice"`
	Observation string                `json:"observation,omitempty"`
	Caveat      string                `json:"caveat"`
	Hooks       []codexHookDiagnostic `json:"hooks"`
}

type codexHookDiagnostic struct {
	SourcePath         string `json:"source_path"`
	Event              string `json:"event"`
	Key                string `json:"key"`
	Status             string `json:"status"`
	Enabled            *bool  `json:"enabled"`
	TrustedHashMatches *bool  `json:"trusted_hash_matches"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message"`
}

type codexHookDefinition struct {
	event string
	key   string
	hash  string
}

func diagnoseCodexHooks(opts *commandOptions, report bus.DiagnosticReport) codexHooksDiagnostic {
	result := codexHooksDiagnostic{
		Status: "cannot_verify", Message: "Codex: cannot verify hook trust.",
		Advice: "Codex: open /hooks and trust the zalki hooks, then send a message in each thread",
		Caveat: "A hash match does not guarantee execution: enabled settings, profile, feature, event, matcher, project policy and a running thread's loaded configuration still apply.",
		Hooks:  []codexHookDiagnostic{},
	}
	if codexZeroSessions(report) {
		result.Observation = "Codex: no Codex sessions are registered."
	}
	if !codexHashVersionSupported(report.Agents) {
		result.Message = "Codex: cannot verify hook trust (unverified version)."
		if root, reason := codexDiagnosticRoot(opts.home); reason == "" {
			if expected, err := expectedCodexHookHashes(opts); err == nil {
				result.Hooks = codexOwnedHookRows(filepath.Join(root, "hooks.json"), expected, nil, nil, "unverified_version")
			}
		}
		return result
	}
	root, reason := codexDiagnosticRoot(opts.home)
	if reason != "" {
		return result
	}
	expected, err := expectedCodexHookHashes(opts)
	if err != nil {
		return result
	}
	path := filepath.Join(root, "hooks.json")
	definitions, reason := readCodexHookDefinitions(path)
	states, stateReason := readCodexHookStates(filepath.Join(root, "config.toml"))
	if reason == "" {
		reason = stateReason
	}
	if reason == "" {
		reason = codexRecordedTargetReason(opts, path)
	}
	result.Hooks = codexOwnedHookRows(path, expected, definitions, states, reason)
	result.Status, result.Message = codexHookSummary(result.Hooks)
	return result
}

func codexHashVersionSupported(agents []bus.AgentDiagnostic) bool {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return false
	}
	for _, client := range agents {
		if client.Kind == "codex" {
			return client.Found && client.Version == "0.167.2"
		}
	}
	return false
}

func codexZeroSessions(report bus.DiagnosticReport) bool {
	if !report.SessionScanComplete {
		return false
	}
	for _, session := range report.Sessions {
		if session.Kind == "codex" {
			return false
		}
	}
	return true
}

func codexDiagnosticRoot(home string) (string, string) {
	root := os.Getenv("CODEX_HOME")
	if !utf8.ValidString(root) || strings.ContainsFunc(root, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		return "", "unsafe_file"
	}
	if strings.TrimSpace(root) == "" {
		root = filepath.Join(home, ".codex")
	}
	if !filepath.IsAbs(root) {
		return "", "unsafe_file"
	}
	return filepath.Clean(root), ""
}

func expectedCodexHookHashes(opts *commandOptions) (map[string]string, error) {
	binary, err := installationBinary()
	if err != nil {
		return nil, err
	}
	files, err := installDeclarations(opts.registry, opts.snapshot.Paths.ConfigFile.Value)(binary, opts.snapshot.Paths.StateDir.Value)
	if err != nil {
		return nil, err
	}
	for _, file := range files {
		if file.Path == ".codex/hooks.json" {
			return expectedCodexHashes(file.JSON)
		}
	}
	return nil, errCodexDefinition
}

func expectedCodexHashes(document map[string]any) (map[string]string, error) {
	definitions, err := parseCodexHookDefinitions("", document)
	if err != nil {
		return nil, err
	}
	expected := map[string]string{}
	for _, definition := range definitions {
		if _, exists := expected[definition.event]; exists {
			return nil, errCodexDefinition
		}
		expected[definition.event] = definition.hash
	}
	return expected, nil
}

func readCodexHookDefinitions(path string) ([]codexHookDefinition, string) {
	raw, reason := readCodexDiagnosticFile(path)
	if reason != "" {
		return nil, reason
	}
	document, err := decodeCodexJSON(raw)
	if err != nil {
		return nil, "invalid_data"
	}
	definitions, err := parseCodexHookDefinitions(path, document)
	if err != nil {
		return nil, "unsupported_definition"
	}
	return definitions, ""
}

func parseCodexHookDefinitions(path string, document map[string]any) ([]codexHookDefinition, error) {
	if !codexKnownFields(document, "hooks", "description") {
		return nil, errCodexDefinition
	}
	if value := document["description"]; value != nil {
		if _, ok := value.(string); !ok {
			return nil, errCodexDefinition
		}
	}
	hooks, ok := document["hooks"].(map[string]any)
	if !ok {
		return nil, errCodexDefinition
	}
	var definitions []codexHookDefinition
	for event, value := range hooks {
		groups, ok := value.([]any)
		if !ok || codexEventKeys[event] == "" {
			return nil, errCodexDefinition
		}
		for index, group := range groups {
			rows, err := codexGroupDefinitions(path, event, index, group)
			if err != nil {
				return nil, err
			}
			definitions = append(definitions, rows...)
		}
	}
	return definitions, nil
}

func codexGroupDefinitions(path, event string, index int, value any) ([]codexHookDefinition, error) {
	group, ok := value.(map[string]any)
	if !ok || !codexKnownFields(group, "matcher", "hooks") {
		return nil, errCodexDefinition
	}
	handlers, ok := group["hooks"].([]any)
	if !ok {
		return nil, errCodexDefinition
	}
	var definitions []codexHookDefinition
	for handlerIndex, value := range handlers {
		handler, ok := value.(map[string]any)
		if !ok {
			return nil, errCodexDefinition
		}
		hash, err := codexHookHash(event, group["matcher"], handler)
		if err != nil {
			return nil, err
		}
		key := fmt.Sprintf("%s:%s:%d:%d", path, codexEventKeys[event], index, handlerIndex)
		definitions = append(definitions, codexHookDefinition{event: event, key: key, hash: hash})
	}
	return definitions, nil
}

// The compiler-generated declaration is the ownership reference, not command
// substrings. Repeated identical owned handlers remain separate state keys.
func codexOwnedHookRows(path string, expected map[string]string, definitions []codexHookDefinition, states map[string]any, reason string) []codexHookDiagnostic {
	events := make([]string, 0, len(expected))
	for event := range expected {
		events = append(events, event)
	}
	sort.Strings(events)
	rows := []codexHookDiagnostic{}
	for _, event := range events {
		found := false
		for _, definition := range definitions {
			if definition.event != event || definition.hash != expected[event] {
				continue
			}
			found = true
			row := codexUnknownHook(path, event, definition.key, reason)
			if reason == "" {
				row = codexHookVerdict(path, definition, states[definition.key])
			}
			rows = append(rows, row)
		}
		if !found {
			missingReason := reason
			if missingReason == "" {
				missingReason = "missing_hook"
			}
			rows = append(rows, codexUnknownHook(path, event, "", missingReason))
		}
	}
	return rows
}

func codexUnknownHook(path, event, key, reason string) codexHookDiagnostic {
	return codexHookDiagnostic{SourcePath: path, Event: event, Key: key, Status: "cannot_verify", Reason: reason,
		Message: "Codex hook " + event + ": cannot verify hook trust."}
}

func codexHookVerdict(path string, definition codexHookDefinition, value any) codexHookDiagnostic {
	row := codexUnknownHook(path, definition.event, definition.key, "invalid_data")
	state := map[string]any{}
	if value != nil {
		var ok bool
		state, ok = value.(map[string]any)
		if !ok || !codexKnownFields(state, "enabled", "trusted_hash", "trusted_content_hash") {
			return row
		}
	}
	enabled, valid := codexStateEnabled(state)
	if !valid {
		return row
	}
	hash, exists, valid := codexSavedHash(state)
	if !valid {
		return row
	}
	match := exists && hash == definition.hash
	row.Enabled, row.TrustedHashMatches, row.Reason = &enabled, &match, ""
	row.Status = "not_trusted"
	message := "not trusted; review it in /hooks."
	if exists {
		row.Status, message = "modified", "hook changed; review it in /hooks."
	}
	if match {
		row.Status, message = "trusted", "trusted hash matches."
	}
	if !enabled {
		row.Status, message = "disabled", "disabled."
	}
	row.Message = "Codex hook " + definition.event + ": " + message
	return row
}

func codexStateEnabled(state map[string]any) (bool, bool) {
	value, exists := state["enabled"]
	if !exists {
		return true, true
	}
	enabled, ok := value.(bool)
	return enabled, ok
}

func codexSavedHash(state map[string]any) (string, bool, bool) {
	if _, exists := state["trusted_content_hash"]; exists {
		// A plugin-content hash is outside this user-file-only comparison.
		return "", false, false
	}
	value, exists := state["trusted_hash"]
	if !exists {
		return "", false, true
	}
	hash, ok := value.(string)
	if !ok || len(hash) != 71 || !strings.HasPrefix(hash, "sha256:") {
		return "", true, false
	}
	for _, r := range hash[7:] {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return "", true, false
		}
	}
	return hash, true, true
}

func codexHookSummary(rows []codexHookDiagnostic) (string, string) {
	if len(rows) == 0 {
		return "cannot_verify", "Codex: cannot verify hook trust."
	}
	for _, row := range rows {
		if row.Status == "cannot_verify" {
			return "cannot_verify", "Codex: cannot verify hook trust."
		}
	}
	return "compared", "Codex: hook trust compared offline; review individual hooks."
}

func codexRecordedTargetReason(opts *commandOptions, path string) string {
	recordPath := filepath.Join(opts.snapshot.Paths.StateDir.Value, "installation.json")
	raw, reason := readCodexDiagnosticFile(recordPath)
	if reason == "missing_file" {
		return "" // Ownership can still be compared to compiled declarations.
	}
	if reason != "" {
		return reason
	}
	record, err := decodeCodexJSON(raw)
	if err != nil {
		return "invalid_data"
	}
	schema, err := codexUnsignedInteger(record["schema"])
	if err != nil || schema != 3 || record["home"] != opts.home || record["state"] != opts.snapshot.Paths.StateDir.Value {
		return "ambiguous_ownership"
	}
	targets, ok := record["targets"].(map[string]any)
	if !ok || targets[".codex/hooks.json"] != path {
		return "ambiguous_ownership"
	}
	return ""
}
