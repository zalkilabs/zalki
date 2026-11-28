package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/statefile"
)

var _ agent.DiagnosticsAdapter = Adapter{}

// maxClientFile bounds what the doctor reads of another tool's settings file.
const maxClientFile = 4 * 1024 * 1024

// diagnostic is the doctor section about Antigravity. Status is "ok",
// "attention" (the problems list says what), "cannot_verify" or "not_installed".
type diagnostic struct {
	Status   string    `json:"status"`
	Message  string    `json:"message"`
	Problems []problem `json:"problems"`
	// Notes are facts the user may want to know that are not problems.
	Notes []string `json:"notes"`
}

// problem is one finding. Code is stable; Message says what is wrong and what to do.
type problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Diagnose reports the doctor section about the Zalki entries in Antigravity's
// files. It reads files and nothing else: it starts no process and writes nothing.
func (a Adapter) Diagnose(in agent.DiagnosticInput) (string, any) {
	return "antigravity", a.diagnose(in)
}

func (a Adapter) diagnose(in agent.DiagnosticInput) diagnostic {
	result := diagnostic{Status: "cannot_verify", Message: "Antigravity: cannot verify the Zalki entries.", Problems: []problem{}, Notes: []string{}}
	if !filepath.IsAbs(in.Binary) || !filepath.IsAbs(in.Home) {
		return result
	}
	if in.RecordReason != "" && in.RecordReason != "missing_file" {
		result.Message = "Antigravity: cannot verify the Zalki entries (" + in.RecordReason + " installation record)."
		return result
	}
	hooks, mcp := targetOf(in, hooksPath), targetOf(in, mcpPath)
	expected := a.Configuration(in.Binary, []string{"--config", in.ConfigFile, "--state", in.StateDir})
	hooksTop, hooksReason := readJSONObject(hooks)
	if in.RecordReason == "missing_file" && (hooksReason == "missing_file" || (hooksReason == "" && hooksTop[ownedEntry] == nil)) {
		result.Status, result.Message = "not_installed", "Antigravity: Zalki is not installed for Antigravity. Run zalki install."
		return result
	}
	result.Problems = append(result.Problems, a.hooksProblems(hooksTop, hooksReason, expected[0].JSON[ownedEntry])...)
	mcpTop, mcpReason := readJSONObject(mcp)
	result.Problems = append(result.Problems, mcpProblems(mcpTop, mcpReason, expected[1].JSON)...)
	if note := toolPermissionNote(in.Home); note != "" {
		result.Notes = append(result.Notes, note)
	}
	if len(result.Problems) == 0 {
		result.Status, result.Message = "ok", "Antigravity: the Zalki hooks and MCP entry are installed."
		return result
	}
	result.Status, result.Message = "attention", "Antigravity: the Zalki entries need attention."
	return result
}

// targetOf is the file the installation record holds for a declared path, or the
// default place under the home directory.
func targetOf(in agent.DiagnosticInput, declared string) string {
	if in.RecordReason == "" {
		if path := in.RecordedTargets[declared]; filepath.IsAbs(path) {
			return path
		}
	}
	return filepath.Join(in.Home, filepath.FromSlash(declared))
}

// readJSONObject reads one settings file as a JSON object, bounded and without
// following a link. The reason is "" on success, else "missing_file", "unsafe_file"
// or "invalid_data".
func readJSONObject(path string) (map[string]any, string) {
	raw, err := statefile.ReadBytes(path, maxClientFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "missing_file"
	}
	if err != nil {
		return nil, "unsafe_file"
	}
	var top map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&top) != nil || top == nil {
		return nil, "invalid_data"
	}
	return top, ""
}

func fileProblem(code, what, reason string) problem {
	return problem{Code: code, Message: "Antigravity: " + what + " is " + strings.ReplaceAll(reason, "_", " ") + ". Run zalki upgrade, or fix the file."}
}

// hooksProblems compares the zalki entry of hooks.json with the expected one.
func (a Adapter) hooksProblems(top map[string]any, reason string, expected any) []problem {
	if reason != "" {
		return []problem{fileProblem("hooks_file_"+reason, "the hooks file", reason)}
	}
	var problems []problem
	entry, present := top[ownedEntry].(map[string]any)
	if !present {
		return []problem{{Code: "entry_missing", Message: "Antigravity: the hooks file has no \"" + ownedEntry + "\" entry. Run zalki upgrade."}}
	}
	want, _ := expected.(map[string]any)
	for _, event := range a.hookEvents() {
		have, found := entry[event]
		switch {
		case !found:
			problems = append(problems, problem{Code: "event_missing", Message: "Antigravity: the \"" + ownedEntry + "\" entry has no " + event + " hook. Run zalki upgrade."})
		case !sameJSON(have, want[event]):
			problems = append(problems, problem{Code: "command_differs", Message: "Antigravity: the " + event + " hook of the \"" + ownedEntry + "\" entry is not the one Zalki wrote. Run zalki upgrade."})
		}
	}
	if enabled, set := entry["enabled"].(bool); set && !enabled {
		problems = append(problems, problem{Code: "entry_disabled", Message: "Antigravity: the \"" + ownedEntry + "\" entry has \"enabled\": false, so Antigravity does not run the Zalki hooks. Remove that key to turn them on."})
	}
	if names := otherEntriesRunningOurHook(top); len(names) > 0 {
		problems = append(problems, problem{Code: "duplicate_hook", Message: "Antigravity: the entries " + strings.Join(names, ", ") + " also run the Zalki hook, so it runs twice. Delete the extra entry."})
	}
	return problems
}

// mcpProblems compares the zalki server of mcp_config.json with the expected one.
func mcpProblems(top map[string]any, reason string, expected map[string]any) []problem {
	if reason != "" {
		return []problem{fileProblem("mcp_file_"+reason, "the MCP file", reason)}
	}
	servers, _ := top["mcpServers"].(map[string]any)
	want, _ := expected["mcpServers"].(map[string]any)
	have, found := servers[ownedEntry]
	switch {
	case !found:
		return []problem{{Code: "mcp_entry_missing", Message: "Antigravity: the MCP file has no \"" + ownedEntry + "\" server. Run zalki upgrade."}}
	case !holdsAll(have, want[ownedEntry]):
		return []problem{{Code: "mcp_entry_differs", Message: "Antigravity: the \"" + ownedEntry + "\" server of the MCP file is not the one Zalki wrote. Run zalki upgrade."}}
	}
	return nil
}

// otherEntriesRunningOurHook names the entries other than ours that have a
// handler running "hook --agent antigravity".
func otherEntriesRunningOurHook(top map[string]any) []string {
	var names []string
	for name, value := range top {
		entry, isEntry := value.(map[string]any)
		if name == ownedEntry || !isEntry {
			continue
		}
		if entryRunsOurHook(entry) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func entryRunsOurHook(entry map[string]any) bool {
	for _, value := range entry {
		list, _ := value.([]any)
		for _, handler := range list {
			if object, ok := handler.(map[string]any); ok {
				if command, _ := object["command"].(string); runsOurHook(command) {
					return true
				}
			}
		}
	}
	return false
}

// runsOurHook reports whether a command line runs "hook --agent antigravity",
// reading words so that quotes or an environment prefix do not hide it.
func runsOurHook(command string) bool {
	words := strings.Fields(strings.NewReplacer("'", " ", `"`, " ").Replace(command))
	for i := len(words) - 2; i >= 0; i-- {
		if words[i] != "hook" {
			continue
		}
		if words[i+1] == "--agent=antigravity" || (words[i+1] == "--agent" && i+2 < len(words) && words[i+2] == "antigravity") {
			return true
		}
	}
	return false
}

// sameJSON compares two decoded JSON values by value (numbers by their digits).
func sameJSON(a, b any) bool {
	left, errA := json.Marshal(a)
	right, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	var x, y any
	return json.Unmarshal(left, &x) == nil && json.Unmarshal(right, &y) == nil && reflect.DeepEqual(x, y)
}

// holdsAll reports whether have is an object that holds every key of want with an equal value.
func holdsAll(have, want any) bool {
	h, okH := have.(map[string]any)
	w, okW := want.(map[string]any)
	if !okH || !okW {
		return false
	}
	for key, value := range w {
		if got, found := h[key]; !found || !sameJSON(got, value) {
			return false
		}
	}
	return true
}

// permissionRulesPath is where agy keeps the permission rules the user saved.
const permissionRulesPath = ".gemini/config/config.json"

// toolPermissionNote says when no allow rule covers the Zalki tools. agy denies a
// tool the user has not allowed, in a headless run at once. The note is quiet when
// the file is missing or has a shape this code does not know, because the rule may
// be kept elsewhere (a rule saved for one project, for example).
func toolPermissionNote(home string) string {
	if !toolPermissionMissing(home) {
		return ""
	}
	return "Antigravity: no allow rule for the Zalki tools was found in " + permissionRulesPath + ". If agy asks to approve a Zalki tool, approve it, or add mcp(" + ownedEntry + "/*) to its allow rules; a headless agy run denies the tools without it."
}

// toolPermissionMissing is true only when the rules file has a shape this code
// knows and no allow rule covers the Zalki tools.
func toolPermissionMissing(home string) bool {
	raw, err := statefile.ReadBytes(filepath.Join(home, filepath.FromSlash(permissionRulesPath)), maxClientFile)
	if err != nil {
		return false
	}
	var top map[string]any
	if json.Unmarshal(raw, &top) != nil || top == nil {
		return false
	}
	allow, known := allowRules(top)
	if !known {
		return false
	}
	for _, rule := range allow {
		if rule == "mcp(*)" || rule == "mcp("+ownedEntry+"/*)" || strings.HasPrefix(rule, "mcp("+ownedEntry+"/") {
			return false
		}
	}
	return true
}

// allowRules returns the allow rules of a settings object. known is false when
// the permissions or their allow list have a shape that is not an object of strings.
func allowRules(top map[string]any) (rules []string, known bool) {
	permissions, present := top["permissions"]
	if !present {
		return nil, true
	}
	object, ok := permissions.(map[string]any)
	if !ok {
		return nil, false
	}
	list, present := object["allow"]
	if !present {
		return nil, true
	}
	items, ok := list.([]any)
	if !ok {
		return nil, false
	}
	for _, item := range items {
		rule, ok := item.(string)
		if !ok {
			return nil, false
		}
		rules = append(rules, rule)
	}
	return rules, true
}
