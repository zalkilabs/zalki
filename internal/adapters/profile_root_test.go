package adapters_test

import (
	"bytes"
	"encoding/json"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func profileVariable(kind string) string {
	return map[string]string{"codex": "CODEX_HOME", "claude": "CLAUDE_CONFIG_DIR", "cursor": "CURSOR_CONFIG_DIR"}[kind]
}
func selectedProfile(t *testing.T, kind string) string {
	t.Helper()
	a, err := adapters.Builtin().Lookup(kind)
	if err != nil {
		t.Fatal(err)
	}
	return a.(agent.NameAdapter).ConfigDir()
}

// clearProfileEnvironment also moves the test into an empty folder and hides
// installed clients: diagnostics run a client's --version, and a real client
// started with an invalid profile (relative or empty config path) writes its
// config file into the current folder.
func clearProfileEnvironment(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
}
func TestNativeProfileSelectionIsAbsoluteAndExact(t *testing.T) {
	for _, kind := range []string{"codex", "claude", "cursor"} {
		t.Run(kind, func(t *testing.T) { checkNativeProfileSelection(t, kind) })
	}
}
func checkNativeProfileSelection(t *testing.T, kind string) {
	t.Helper()
	clearProfileEnvironment(t)
	checkProfileCharacterPolicy(t, kind)
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := selectedProfile(t, kind); got != filepath.Join(home, "."+kind) {
		t.Fatal("PROFILE_DEFAULT_CHANGED", got)
	}
	for _, bad := range []string{"", "relative-home"} {
		t.Setenv("HOME", bad)
		if got := selectedProfile(t, kind); got != "" {
			t.Fatal("INVALID_HOME_SELECTED_PROFILE", got)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv(profileVariable(kind), "relative-profile")
	if got := selectedProfile(t, kind); got != "" {
		t.Fatal("RELATIVE_OVERRIDE_USED_PROCESS_CWD", got)
	}
	exact := filepath.Join(t.TempDir(), " profile with spaces ")
	t.Setenv(profileVariable(kind), exact)
	if got := selectedProfile(t, kind); got != exact {
		t.Fatal("PROFILE_PATH_SPACES_CHANGED", got)
	}
	t.Setenv("HOME", "")
	if got := selectedProfile(t, kind); got != exact {
		t.Fatal("ABSOLUTE_OVERRIDE_REQUIRES_HOME", got)
	}
	t.Setenv(profileVariable(kind), "")
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if kind == "cursor" && selectedProfile(t, kind) != filepath.Join(xdg, "cursor") {
		t.Fatal("CURSOR_XDG_PRECEDENCE_CHANGED")
	}
}
func TestNativeProfileOverrideDoesNotFallBackWithMissingCWD(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	t.Chdir(cwd)
	if err := os.Remove(cwd); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"codex", "claude", "cursor"} {
		clearProfileEnvironment(t)
		t.Setenv("HOME", home)
		t.Setenv(profileVariable(kind), "relative-profile")
		if got := selectedProfile(t, kind); got != "" {
			t.Fatal("FAILED_OVERRIDE_FELL_BACK", kind, got)
		}
	}
}
func TestNativeProfileCreationRoutes(t *testing.T) {
	for _, kind := range []string{"codex", "claude", "cursor"} {
		for _, hook := range []bool{false, true} {
			for _, mode := range []string{"absolute", "relative", "missing-home"} {
				name := kind + "/" + mode + map[bool]string{false: "/direct", true: "/hook"}[hook]
				t.Run(name, func(t *testing.T) { checkProfileCreationRoute(t, kind, mode, hook) })
			}
		}
	}
}
func checkProfileCreationRoute(t *testing.T, kind, mode string, hook bool) {
	t.Helper()
	clearProfileEnvironment(t)
	t.Setenv("HOME", t.TempDir())
	root := filepath.Join(t.TempDir(), " profile ")
	t.Setenv(profileVariable(kind), root)
	if mode == "relative" {
		t.Setenv(profileVariable(kind), "profile-value-secret")
	}
	if mode == "missing-home" {
		t.Setenv("HOME", "")
		t.Setenv(profileVariable(kind), "")
	}
	s := adapterTestStore(t)
	id := model.NewUUID()
	var err error
	if hook {
		err = profileStartHook(t, s, kind, id)
	} else {
		var endpoint *model.Endpoint
		if kind == "claude" {
			endpoint = &model.Endpoint{Path: "/fixture/socket"}
		}
		_, _, err = s.Register(kind, id, "/workspace", endpoint)
	}
	if mode != "absolute" {
		if model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatal("INVALID_NATIVE_PROFILE_ROUTE_REGISTERED", kind, mode, hook, err)
		}
		checkInvalidProfileGuidance(t, s, kind, err)
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if current := adapterTestSession(t, s, kind+":"+id); current.NativeConfigDir != root {
		t.Fatal("NATIVE_PROFILE_NOT_CAPTURED", current.NativeConfigDir)
	}
}
func profileStartHook(t *testing.T, s bus.Store, kind, id string) error {
	t.Helper()
	event := "SessionStart"
	if kind == "cursor" {
		event = "sessionStart"
	}
	if kind == "claude" {
		_, endpoint := listenClaudeSocket(t)
		t.Setenv("CLAUDE_CODE_SESSION_ID", id)
		t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", endpoint.Path)
	}
	raw, err := json.Marshal(agent.HookInput{SessionID: id, CWD: "/workspace", Event: event})
	if err != nil {
		t.Fatal(err)
	}
	return (&bus.Service{Store: s}).Hook(t.Context(), kind, bytes.NewReader(raw), &bytes.Buffer{})
}
func TestCursorPromptProfileRefusalDoesNotDebitQuota(t *testing.T) {
	clearProfileEnvironment(t)
	t.Setenv("HOME", "")
	s := adapterTestStore(t)
	id := model.NewUUID()
	raw, _ := json.Marshal(map[string]string{"conversation_id": id, "cwd": "/workspace", "hook_event_name": "beforeSubmitPrompt"})
	var output bytes.Buffer
	if err := (&bus.Service{Store: s}).Hook(t.Context(), "cursor", bytes.NewReader(raw), &output); err != nil || !strings.Contains(output.String(), `"continue":true`) {
		t.Fatal("PROFILE_REFUSAL_BLOCKED_PROMPT", err, output.String())
	}
	for _, path := range []string{filepath.Join(s.Dir(), "sessions", "cursor_"+id+".json"), filepath.Join(s.Dir(), "hook-registration-quota.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("INVALID_PROMPT_PROFILE_PUBLISHED_OR_DEBITED", path, err)
		}
	}
}
func TestNativeNameLookupNeverUsesForeignProfile(t *testing.T) {
	for _, kind := range []string{"codex", "claude", "cursor"} {
		t.Run(kind, func(t *testing.T) {
			root := nameFixture(t)
			a, _ := adapters.Builtin().Lookup(kind)
			v := model.Session{Kind: kind, ID: kind + ":" + model.NewUUID()}
			v.NativeID = strings.TrimPrefix(v.ID, kind+":")
			nativeNameFixture(t, root, v, "foreign profile title")
			for _, bad := range []string{"", "relative"} {
				v.NativeConfigDir = bad
				if name, _ := a.(agent.NameAdapter).LookupName(v, map[string]map[string]string{}); name != "" {
					t.Fatal("FOREIGN_PROFILE_NAME_READ", name)
				}
			}
		})
	}
}

func checkProfileCharacterPolicy(t *testing.T, kind string) {
	t.Helper()
	for _, suffix := range []string{"soft\u00adhyphen", "zero\u200djoiner", " spaces "} {
		exact := filepath.Join(t.TempDir(), suffix)
		t.Setenv(profileVariable(kind), exact)
		if got := selectedProfile(t, kind); got != exact {
			t.Fatal("CONTROL_FREE_PROFILE_WAS_REFUSED", kind, got)
		}
	}
	for _, bad := range []string{"~/profile", "relative-profile", "/bad\nprofile", "/bad\x7fprofile", "/bad\x85profile", "/bad\xffprofile"} {
		t.Setenv(profileVariable(kind), bad)
		if got := selectedProfile(t, kind); got != "" {
			t.Fatal("INVALID_PROFILE_CHARACTER_ACCEPTED", kind, got)
		}
	}
	t.Setenv(profileVariable(kind), "")
}

func checkInvalidProfileGuidance(t *testing.T, s bus.Store, kind string, err error) {
	t.Helper()
	text := err.Error()
	if !strings.Contains(text, profileVariable(kind)) || !strings.Contains(text, "HOME") || strings.Contains(text, "profile-value-secret") || strings.Contains(text, "run zalki doctor") {
		t.Fatal("PROFILE_REFUSAL_LOST_VARIABLE_GUIDANCE", text)
	}
	if kind == "cursor" && !strings.Contains(text, "XDG_CONFIG_HOME") {
		t.Fatal("CURSOR_PROFILE_REFUSAL_LOST_XDG_GUIDANCE", text)
	}
	report, doctorErr := s.Diagnostics(t.Context())
	notes := strings.Join(report.Notes, "\n")
	if doctorErr != nil || !strings.Contains(notes, "Selected client profile is invalid.") || !strings.Contains(notes, profileVariable(kind)) || strings.Contains(notes, "profile-value-secret") {
		t.Fatal("DOCTOR_LOST_INVALID_PROFILE_SELECTION", doctorErr, notes)
	}
}
