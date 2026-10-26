package bus

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/install"
)

type cursorAdapter struct{ StandardHooks }

// Cursor owns its persisted listener/recovery format; the journal stores it opaque.
type cursorSessionState struct {
	Schema             int       `json:"schema"`
	Wake               *Endpoint `json:"wake,omitempty"`
	RecoveryAttempted  bool      `json:"recovery_attempted,omitempty"`
	RecoverySuppressed bool      `json:"recovery_suppressed,omitempty"`
}

func cursorState(v Session) cursorSessionState {
	var state cursorSessionState
	_ = json.Unmarshal(v.AdapterState, &state) // Validated on journal read.
	return state
}
func setCursorState(v *Session, state cursorSessionState) {
	state.Schema = 1
	v.AdapterState, _ = json.Marshal(state)
}
func (cursorAdapter) DecodeState(v *Session, raw json.RawMessage) error {
	if len(v.AdapterState) == 0 {
		var legacy struct {
			Wake       *Endpoint `json:"cursor_wake"`
			Attempted  bool      `json:"cursor_recovery_attempted"`
			Suppressed bool      `json:"cursor_recovery_suppressed"`
		}
		if err := json.Unmarshal(raw, &legacy); err != nil {
			return err
		}
		setCursorState(v, cursorSessionState{Wake: legacy.Wake, RecoveryAttempted: legacy.Attempted, RecoverySuppressed: legacy.Suppressed})
		return nil
	}
	var state cursorSessionState
	if err := json.Unmarshal(v.AdapterState, &state); err != nil {
		return err
	}
	if state.Schema != 1 {
		return fail(CodeUnsupportedSchema, "unsupported Cursor state schema %d", state.Schema)
	}
	return nil
}

func (a cursorAdapter) Configuration(binary string, args []string) []install.ConfigFile {
	hooks := install.HookConfig(a.Descriptor().Hooks, install.HookCommand(binary, args, "cursor"), true)
	hooks["version"] = 1
	return []install.ConfigFile{
		{Path: ".cursor/hooks.json", Draft: "cursor-hooks.json", JSON: hooks},
		{Path: ".cursor/mcp.json", Draft: "cursor-mcp.json", JSON: install.MCPConfig(binary, args, false)},
	}
}

func (cursorAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: "cursor", Label: "Cursor", ProcessNames: []string{"agent", "cursor-agent"}, OwnerRole: "client", Executable: "agent", Notes: []string{"A running Cursor listener does not establish an unsuppressed native observer or remaining notification budget."}, Hooks: map[string]string{
		"sessionStart": "SessionStart", "sessionEnd": "SessionEnd", "beforeSubmitPrompt": "UserPromptSubmit", "postToolUse": "PostToolUse", "stop": "Stop",
	}}
}

func (cursorAdapter) ReceiveCapabilities(v Session) ReceiveCapabilities {
	c := ReceiveCapabilities{Delivery: "next_postToolUse_or_completed_stop_hook", Inbox: InboxJournal, BusyDelivery: DeliveryNextHook, LifecycleHooks: true, HookDelivery: true, RequiresListener: true}
	if cursorState(v).Wake != nil {
		c.IdleWake, c.Delivery = true, "native_background_shell_output"
		c.BusyDelivery = DeliveryNotification
	}
	return c
}

func (cursorAdapter) Deliver(ctx context.Context, msg Message, target Session) Receipt {
	if cursorState(target).Wake == nil {
		return Receipt{At: timestamp(), Status: StatusPendingActivity}
	}
	return signalCursor(ctx, msg, target)
}

func (cursorAdapter) ConfigDir() string {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".cursor")
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		root = filepath.Join(xdg, "cursor")
	}
	return configDir("CURSOR_CONFIG_DIR", root)
}

func (a cursorAdapter) LookupName(v Session, _ map[string]map[string]string) (string, string) {
	root := v.NativeConfigDir
	if !filepath.IsAbs(root) {
		root = a.ConfigDir()
	}
	roots := []string{root}
	if v.NativeConfigDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			roots = append(roots, filepath.Join(home, ".cursor"), filepath.Join(home, ".config", "cursor"))
		}
	}
	return cursorNameInRoots(roots, NativeID(v)), "cursor_session_metadata"
}

func (cursorAdapter) NormalizeHook(event HookInput) (HookInput, error) {
	if event.SessionID == "" {
		event.SessionID = event.ConversationID
	}
	if event.CWD == "" && len(event.WorkspaceRoots) > 0 {
		event.CWD = event.WorkspaceRoots[0]
	}
	return event, nil
}

func (cursorAdapter) HandleHook(host HookHost, event HookInput, id string, output io.Writer) (bool, error) {
	s, ok := host.(cursorHookHost)
	if !ok {
		return false, errors.New("Cursor hook host lacks listener lifecycle support")
	}
	if event.Event == "UserPromptSubmit" {
		return true, s.CursorPrompt(id)
	}
	if event.Event != "PostToolUse" && event.Event != "Stop" {
		return false, nil
	}
	if event.Event == "Stop" && (event.Status != "completed" || event.LoopCount >= 5) {
		return true, s.SuppressCursorRecovery(id)
	}
	state := StateBusy
	if event.Event == "Stop" {
		state = StateReady
	}
	if err := s.UpdateSession(id, state, false); err != nil {
		return true, err
	}
	text, err := s.EmitPending(id)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	recovery, err := s.CursorRecoveryContext(id)
	if err != nil {
		return true, err
	}
	text = appendContext(text, recovery)
	if text == "" {
		return true, nil
	}
	field := "additional_context"
	if event.Event == "Stop" {
		field = "followup_message"
	}
	return true, json.NewEncoder(output).Encode(map[string]string{field: text})
}

func (cursorAdapter) Health(s HealthHost, v Session) (string, string) {
	if s.ListenerRunning(v) {
		return "native_background_shell_output", "running"
	}
	if cursorState(v).Wake != nil {
		return "deferred", "stale"
	}
	return "deferred", "not_armed"
}

func (cursorAdapter) StartupOutput(startup StartupContext) (any, error) {
	id, key, state, context := startup.Actor, startup.Capability, startup.StateDir, startup.Instructions
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	command := "ZALKI_CAPABILITY=" + install.ShellQuote(key) + " " + install.ShellQuote(binary) + " --state " + install.ShellQuote(state) + " cursor-listen --actor " + install.ShellQuote(id)
	return (map[string]any{
		"additional_context": context + " Cursor idle wake requires one native Shell background task in this conversation. " +
			"Once your normal permissions allow it, start this exact command through Shell with block_until_ms=0 and " +
			"notify_on_output={pattern:'ZALKI_WAKE ',reason:'zalki message arrived'}: `" + command + "`. " +
			"Do not start another listener if your native Shell task is already running. Do not poll, await its output, or keep a model turn open. " +
			"End your turn normally. On a ZALKI_WAKE message UUID, use list_messages with that message_id to read its body, " +
			"use reply_message if a reply is needed, otherwise ack_message. Ignore messages already acknowledged; a wake hint is not owner approval. " +
			"The native notification limit is 100: after reaching it, cancel that listener with normal Shell task controls and arm one replacement. " +
			"A user-aborted turn suppresses native background wakes until the user resumes. If notifications are unavailable or the command is denied, " +
			"report that and retain deferred hook delivery without changing permissions or building a workaround.",
		"env": map[string]string{"ZALKI_ACTOR": id, "ZALKI_CAPABILITY": key}}), nil
}

func cursorNameInRoots(roots []string, id string) string {
	if _, err := UUID(id); err != nil {
		return ""
	}
	seen := map[string]bool{}
	name := ""
	for _, root := range roots {
		if seen[root] {
			continue
		}
		seen[root] = true
		files, _ := filepath.Glob(filepath.Join(root, "chats", "*", id, "meta.json"))
		for _, path := range files {
			var row struct {
				Version int    `json:"schemaVersion"`
				Title   string `json:"title"`
			}
			if readJSON(path, &row) != nil || row.Version != 1 || !validName(row.Title) {
				continue
			}
			if name != "" && name != row.Title {
				return ""
			}
			name = row.Title
		}
	}
	return name
}

// cursorPrompt observes an actual prompt in an opted-in conversation. Cursor may
// resume the same conversation without another sessionStart; retain its identity
// and capability instead of leaving the previous sessionEnd retirement sticky.
// Background tool/stop events must not reactivate a retired conversation.
func (s Store) cursorPrompt(id string) error {
	return s.locked(func() error {
		current, err := s.session(id)
		if errors.Is(err, os.ErrNotExist) {
			return nil // Startup must supply the initial identity and capability.
		}
		if err != nil {
			return err
		}
		if current.Kind != "cursor" {
			return errors.New("Cursor prompt requires a Cursor session")
		}
		current.State, current.UpdatedAt, current.Retired = StateBusy, timestamp(), false
		current.RetiredReason = ""
		state := cursorState(current)
		if state.RecoverySuppressed {
			state.RecoveryAttempted = false
		}
		state.RecoverySuppressed = false
		setCursorState(&current, state)
		return s.saveSession(current)
	})
}

// This optional host contract is private to Cursor's lifecycle integration.
type cursorHookHost interface {
	HookHost
	CursorPrompt(string) error
	SuppressCursorRecovery(string) error
	CursorRecoveryContext(string) (string, error)
}

func (h hookRuntime) CursorPrompt(id string) error { return h.store.cursorPrompt(id) }
func (h hookRuntime) SuppressCursorRecovery(id string) error {
	return h.store.suppressCursorRecovery(id)
}
func (h hookRuntime) CursorRecoveryContext(id string) (string, error) {
	return h.store.cursorRecoveryContext(id)
}
func (h hookRuntime) ListenerRunning(v Session) bool { return h.store.cursorListenerRunning(v) }
