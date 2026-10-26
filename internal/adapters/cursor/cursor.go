package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
)

// Adapter implements the Cursor client.
type Adapter struct{ bus.StandardHooks }

// New returns the Cursor adapter.
func New() Adapter { return Adapter{} }

func (a Adapter) Configuration(binary string, args []string) []install.ConfigFile {
	hooks := install.HookConfig(a.Descriptor().Hooks, install.HookCommand(binary, args, "cursor"), true)
	hooks["version"] = 1
	return []install.ConfigFile{
		{Path: ".cursor/hooks.json", Draft: "cursor-hooks.json", JSON: hooks},
		{Path: ".cursor/mcp.json", Draft: "cursor-mcp.json", JSON: install.MCPConfig(binary, args, false)},
	}
}

func (Adapter) Descriptor() bus.AdapterDescriptor {
	return bus.AdapterDescriptor{Kind: "cursor", Label: "Cursor", ProcessNames: []string{"agent", "cursor-agent"}, OwnerRole: "client", Executable: "agent", Notes: []string{"A running Cursor listener does not establish an unsuppressed native observer or remaining notification budget."}, Hooks: map[string]string{
		"sessionStart": "SessionStart", "sessionEnd": "SessionEnd", "beforeSubmitPrompt": "UserPromptSubmit", "postToolUse": "PostToolUse", "stop": "Stop",
	}}
}

func (Adapter) ReceiveCapabilities(v bus.Session) bus.ReceiveCapabilities {
	c := bus.ReceiveCapabilities{Delivery: "next_postToolUse_or_completed_stop_hook", Inbox: bus.InboxJournal, BusyDelivery: bus.DeliveryNextHook, LifecycleHooks: true, HookDelivery: true, RequiresListener: true}
	if cursorState(v).Wake != nil {
		c.IdleWake, c.Delivery = true, "native_background_shell_output"
		c.BusyDelivery = bus.DeliveryNotification
	}
	return c
}

func (Adapter) Deliver(ctx context.Context, msg bus.Message, target bus.Session) bus.Receipt {
	if cursorState(target).Wake == nil {
		return bus.NewReceipt(bus.StatusPendingActivity)
	}
	return signalCursor(ctx, msg, target)
}

func (Adapter) ConfigDir() string {
	home, _ := os.UserHomeDir()
	root := filepath.Join(home, ".cursor")
	if xdg := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(xdg) {
		root = filepath.Join(xdg, "cursor")
	}
	return configDir("CURSOR_CONFIG_DIR", root)
}

func (a Adapter) LookupName(v bus.Session, _ map[string]map[string]string) (string, string) {
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
	return cursorNameInRoots(roots, bus.NativeID(v)), "cursor_session_metadata"
}

func (Adapter) NormalizeHook(event bus.HookInput) (bus.HookInput, error) {
	if event.SessionID == "" {
		event.SessionID = event.ConversationID
	}
	if event.CWD == "" && len(event.WorkspaceRoots) > 0 {
		event.CWD = event.WorkspaceRoots[0]
	}
	return event, nil
}

func (Adapter) HandleHook(host bus.HookHost, event bus.HookInput, id string, output io.Writer) (bool, error) {
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
	state := bus.StateBusy
	if event.Event == "Stop" {
		state = bus.StateReady
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
	binary, err := os.Executable()
	if err != nil {
		return true, err
	}
	claim, err := s.ClaimCursorRecovery(id)
	if err != nil {
		return true, err
	}
	text = strings.TrimSpace(text + "\n\n" + cursorRecoveryContext(binary, claim))
	if text == "" {
		return true, nil
	}
	field := "additional_context"
	if event.Event == "Stop" {
		field = "followup_message"
	}
	return true, json.NewEncoder(output).Encode(map[string]string{field: text})
}

func (Adapter) Health(s bus.HealthHost, v bus.Session) (string, string) {
	if s.ListenerRunning(v) {
		return "native_background_shell_output", "running"
	}
	if cursorState(v).Wake != nil {
		return "deferred", "stale"
	}
	return "deferred", "not_armed"
}

func (Adapter) StartupOutput(startup bus.StartupContext) (any, error) {
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
	if _, err := bus.UUID(id); err != nil {
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
			if readCursorMetadata(path, &row) != nil || row.Version != 1 || !validCursorName(row.Title) {
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

// cursorHookHost is satisfied by the trusted core hook runtime. These are
// intent operations, not journal access or a callable Store credential API.
type cursorHookHost interface {
	bus.HookHost
	CursorPrompt(string) error
	SuppressCursorRecovery(string) error
	ClaimCursorRecovery(string) (bus.CursorRecovery, error)
}

func (Adapter) DecodeState(v *bus.Session, raw json.RawMessage) error {
	return (bus.CursorListenerState{}).Restore(v, raw)
}

func cursorState(v bus.Session) bus.CursorListenerState {
	var state bus.CursorListenerState
	_ = json.Unmarshal(v.AdapterState, &state) // Core validates on journal read.
	return state
}

// Cursor metadata has the same 128 KiB bound as the former journal reader, but
// is client data rather than a journal record. Reject symlinks and trailing JSON.
func readCursorMetadata(path string, target any) error {
	const limit = 128 * 1024
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return errors.New("invalid Cursor metadata file")
	}
	f, err := os.Open(path) //nolint:gosec // G304: path is under the recipient's Cursor profile.
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // Read-only metadata lookup has no pending writes.
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if len(data) > limit {
		return errors.New("Cursor metadata file too large")
	}
	return json.Unmarshal(data, target)
}

// Filter before conflict detection; an invalid label must not hide a valid one.
// Core also validates labels returned by adapters before exposing them.
func validCursorName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && len(name) <= 512 && utf8.ValidString(name) &&
		!strings.ContainsFunc(name, unicode.IsControl)
}

func configDir(variable, fallback string) string {
	if value := os.Getenv(variable); strings.TrimSpace(value) != "" {
		if path, err := filepath.Abs(value); err == nil {
			return path
		}
	}
	return fallback
}
