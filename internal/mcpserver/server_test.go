package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestOfficialSDKDiscoveryAndToolValidation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	service := &bus.Service{Store: bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: adapters.Builtin()}}
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(service).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()
	client := mcp.NewClient(&mcp.Implementation{Name: "zalki-test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 5 {
		t.Fatalf("found %d tools", len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		encoded, err := json.Marshal(tool.OutputSchema)
		if err != nil {
			t.Fatal(err)
		}
		if tool.Name == "list_agents" {
			for _, private := range []string{"adapter_state", "native_id", "key_hash", "endpoint", "native_config_dir"} {
				if bytes.Contains(encoded, []byte(`"`+private+`"`)) {
					t.Fatalf("discovery schema exposes internal field %s", private)
				}
			}
		}
		if tool.Name == "list_messages" {
			for _, internal := range []string{"schema", "target_generation", "requested_to", "receipt"} {
				if bytes.Contains(encoded, []byte(`"`+internal+`"`)) {
					t.Fatalf("inbox schema repeats journal/transport field %s", internal)
				}
			}
		}
		var schema struct {
			Properties map[string]struct {
				Type       json.RawMessage            `json:"type"`
				Items      map[string]json.RawMessage `json:"items"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(encoded, &schema); err != nil {
			t.Fatalf("%s has an incompatible output schema: %s: %v", tool.Name, encoded, err)
		}
		data := schema.Properties["data"]
		if tool.Name == "list_agents" {
			if !bytes.Contains(data.Type, []byte(`"array"`)) || string(data.Items["type"]) != `"object"` {
				t.Fatalf("%s must describe an array of records: %s", tool.Name, encoded)
			}
		} else if tool.Name == "list_messages" {
			if string(data.Type) != `"object"` || !bytes.Contains(data.Properties["messages"], []byte(`"array"`)) || !bytes.Contains(data.Properties["more"], []byte(`"boolean"`)) {
				t.Fatalf("inbox must expose a typed bounded page: %s", encoded)
			}
		} else if string(data.Type) != `"object"` || string(data.Properties["id"]) != `{"type":"string"}` {
			t.Fatalf("%s must describe a message record: %s", tool.Name, encoded)
		}
	}
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatalf("%+v %v", result, err)
	}
	encodedRuntime, _ := json.Marshal(result.StructuredContent)
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(encodedRuntime, &envelope); err != nil || envelope["runtime"] != nil {
		t.Fatalf("healthy result repeated runtime diagnostics: %s %v", encodedRuntime, err)
	}
	if session.InitializeResult().ServerInfo.Version != bus.BuildVersion() {
		t.Fatal("loaded build missing from MCP initialization")
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"body": "missing required identity"}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("SDK accepted missing required tool arguments")
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{"unexpected": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("SDK accepted unknown properties")
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"actor": "fixture", "capability": "fixture", "to": "fixture", "message_id": "fixture", "body": "reply", "reply_to": "fixture",
	}})
	if err != nil || !result.IsError {
		t.Fatal("obsolete manual reply routing should be rejected; use reply_message", err)
	}
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"actor": "fixture", "capability": "fixture", "to": "fixture", "message_id": "not-a-uuid", "body": "ping",
	}})
	if err != nil || !result.IsError {
		t.Fatal("invalid identity was accepted", err)
	}
	encoded, _ := json.Marshal(result.StructuredContent)
	var failure struct {
		Error ToolError `json:"error"`
	}
	if err := json.Unmarshal(encoded, &failure); err != nil || failure.Error.Code != bus.CodeInvalidIdentity {
		t.Fatalf("domain failure has no structured stable code: %s %v", encoded, err)
	}
}

func TestBinaryLoadedBuildSurvivesOnDiskUpgrade(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("set ZALKI_TEST_BINARY for stdio upgrade integration")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "zalki")
	bytesOnDisk, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(copyPath, bytesOnDisk, 0700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	store := bus.Store{Dir: state, Registry: adapters.Builtin()}
	actor, key, err := store.Register("cursor", "00000000-0000-4000-8000-000000000011", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "upgrade-fixture", Version: "0"}, nil)
	serverCommand := exec.CommandContext(ctx, copyPath, "--state", state, "mcp")
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: serverCommand}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	call := func() *bus.RuntimeStatus {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_messages", Arguments: map[string]any{"actor": actor.ID, "capability": key}})
		if err != nil || result.IsError {
			t.Fatalf("runtime call failed: %+v %v", result, err)
		}
		b, _ := json.Marshal(result.StructuredContent)
		var value struct {
			Runtime *bus.RuntimeStatus `json:"runtime"`
		}
		if err := json.Unmarshal(b, &value); err != nil {
			t.Fatal(err)
		}
		return value.Runtime
	}
	if warning := call(); warning != nil {
		t.Fatal("healthy tool result included runtime diagnostics")
	}
	loadedHash := sha256.Sum256(bytesOnDisk)
	before := bus.RuntimeStatus{PID: serverCommand.Process.Pid, LoadedBuild: hex.EncodeToString(loadedHash[:])}
	if !strings.HasSuffix(session.InitializeResult().ServerInfo.Version, "+"+before.LoadedBuild[:12]) {
		t.Fatal("MCP initialization lost the loaded build identity")
	}
	// Atomic replacement is how Go installs a rebuilt binary. Appending fixture
	// bytes gives a distinct installed hash while the existing executable image
	// and its MCP connection remain alive. The replacement is never executed.
	newPath := copyPath + ".new"
	if err := os.WriteFile(newPath, append(bytesOnDisk, []byte("\nupgrade fixture\n")...), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(newPath, copyPath); err != nil {
		t.Fatal(err)
	}
	after := call()
	if after == nil || !after.NeedsRestart || after.LoadedBuild != before.LoadedBuild || after.PID != before.PID || after.DiskBuild == before.LoadedBuild {
		t.Fatalf("old process relabeled itself after upgrade: before=%+v after=%+v", before, after)
	}
	command := exec.CommandContext(ctx, binary, "--state", state, "doctor")
	out, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Runtimes []bus.RuntimeInfo `json:"runtimes"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Runtimes) != 1 || report.Runtimes[0].PID != before.PID || !report.Runtimes[0].NeedsRestart || len(report.Runtimes[0].Actors) != 1 || report.Runtimes[0].Actors[0] != actor.ID {
		t.Fatalf("doctor lost MCP runtime association: %+v", report.Runtimes)
	}
	if strings.Contains(string(out), key) || strings.Contains(string(out), actor.KeyHash) {
		t.Fatal("doctor leaked capability data")
	}
	for _, dry := range []bool{true, false} {
		args := []string{"--state", state, "prune", "--session", actor.ID}
		if dry {
			args = append(args, "--dry-run")
		}
		out, err := exec.CommandContext(ctx, binary, args...).Output()
		if err != nil {
			t.Fatal(err)
		}
		var report bus.PruneReport
		if err := json.Unmarshal(out, &report); err != nil || len(report.Sessions) != 1 || report.DryRun != dry {
			t.Fatalf("bad CLI prune report: %s %v", out, err)
		}
		items, err := store.List()
		if err != nil || (dry && len(items) != 1) || (!dry && len(items) != 0) {
			t.Fatal("CLI prune did not respect preview/apply", err)
		}
	}
}

func TestBinarySTDIOAndCursorHook(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("set ZALKI_TEST_BINARY to exercise the built binary over stdio")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dir := filepath.Join(t.TempDir(), "state")
	store := bus.Store{Dir: dir, Registry: adapters.Builtin()}
	cursorConfig := filepath.Join(t.TempDir(), "cursor")
	t.Setenv("CURSOR_CONFIG_DIR", cursorConfig)
	from, key, err := store.Register("cursor", "00000000-0000-4000-8000-000000000001", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	to, toKey, err := store.Register("cursor", "00000000-0000-4000-8000-000000000002", "/fixture", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise native-name discovery and resolution through the actual stdio
	// transport, not just a direct Store call. No provider process is contacted.
	metadata := filepath.Join(cursorConfig, "chats", "fixture", "00000000-0000-4000-8000-000000000002", "meta.json")
	if err := os.MkdirAll(filepath.Dir(metadata), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(metadata, []byte(`{"schemaVersion":1,"title":"fixture receiver"}`), 0600); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "zalki-binary-test", Version: "0"}, nil)
	command := exec.CommandContext(ctx, binary, "--state", dir, "mcp")
	// The sender MCP intentionally lacks the receiver's config environment.
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	id := "00000000-0000-4000-8000-000000000003"
	result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{
		"actor": from.ID, "capability": key, "to": "cursor:fixture receiver", "message_id": id, "body": "fixture peer note",
	}})
	if err != nil || result.IsError {
		t.Fatalf("%+v %v", result, err)
	}
	items, err := store.Messages(to.ID, toKey)
	if err != nil || len(items) != 1 || items[0].Status != "pending_next_activity" || items[0].To != to.ID || items[0].RequestedTo != "cursor:fixture receiver" {
		t.Fatalf("%+v %v", items, err)
	}
	read := func(actor, capability string, history bool) []MessageView {
		t.Helper()
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_messages", Arguments: map[string]any{
			"actor": actor, "capability": capability, "history": history,
		}})
		if err != nil || result.IsError {
			t.Fatalf("inbox failed: %+v %v", result, err)
		}
		encoded, _ := json.Marshal(result.StructuredContent)
		var value struct {
			Data MessagePage `json:"data"`
		}
		if err := json.Unmarshal(encoded, &value); err != nil {
			t.Fatal(err)
		}
		for _, hidden := range []string{"target_generation", "requested_to", "receipt", "runtime"} {
			if bytes.Contains(encoded, []byte(`"`+hidden+`"`)) {
				t.Fatalf("inbox repeated internal metadata: %s", encoded)
			}
		}
		return value.Data.Messages
	}
	if inbox := read(to.ID, toKey, false); len(inbox) != 1 || inbox[0].ID != id || inbox[0].Body != "fixture peer note" {
		t.Fatal("unread inbox lost the incoming body")
	}
	if len(read(from.ID, key, false)) != 0 || len(read(from.ID, key, true)) != 1 {
		t.Fatal("sent history leaked into the default inbox or explicit history was lost")
	}
	hook := exec.CommandContext(ctx, binary, "--state", dir, "hook", "--agent", "cursor")
	hook.Stdin = bytes.NewBufferString(`{"conversation_id":"00000000-0000-4000-8000-000000000002","hook_event_name":"postToolUse"}`)
	out, err := hook.Output()
	if err != nil {
		t.Fatal(err)
	}
	var contextOutput map[string]string
	if err := json.Unmarshal(out, &contextOutput); err != nil {
		t.Fatal(err)
	}
	if contextOutput["additional_context"] == "" {
		t.Fatalf("missing hook context: %s", out)
	}
	// The trusted hook reissues only this receiver's capability during recovery.
	// The receiving model gets it in its private additional context.
	match := regexp.MustCompile(`refreshed private capability is ([a-f0-9-]+);`).FindStringSubmatch(contextOutput["additional_context"])
	if len(match) != 2 {
		t.Fatal("missing recovery credentials in receiver hook context")
	}
	toKey = match[1]
	result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "ack_message", Arguments: map[string]any{
		"actor": to.ID, "capability": toKey, "message_id": id,
	}})
	if err != nil || result.IsError {
		t.Fatalf("%+v %v", result, err)
	}
	items, err = store.Messages(to.ID, toKey)
	if err != nil || len(items) != 1 || items[0].AcknowledgedAt == "" {
		t.Fatalf("ACK lost receipt or generated another message: %+v %v", items, err)
	}
	ackJSON, _ := json.Marshal(result.StructuredContent)
	var ack struct {
		Data MessageReceipt `json:"data"`
	}
	if err := json.Unmarshal(ackJSON, &ack); err != nil || !ack.Data.Acknowledged || bytes.Contains(ackJSON, []byte("fixture peer note")) || bytes.Contains(ackJSON, []byte(`"runtime"`)) {
		t.Fatalf("ACK did not return a compact local receipt: %s %v", ackJSON, err)
	}
	if len(read(to.ID, toKey, false)) != 0 || len(read(to.ID, toKey, true)) != 1 {
		t.Fatal("acknowledged message repeated in inbox or disappeared from history")
	}
	for _, limit := range []int{0, -1, 101} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_messages", Arguments: map[string]any{
			"actor": to.ID, "capability": toKey, "limit": limit,
		}})
		if err != nil || !result.IsError {
			t.Fatal("SDK accepted invalid inbox limit", err)
		}
	}
	var replyID string
	for range 2 {
		result, err = session.CallTool(ctx, &mcp.CallToolParams{Name: "reply_message", Arguments: map[string]any{
			"actor": to.ID, "capability": toKey, "message_id": id, "body": "fixture reply",
		}})
		if err != nil || result.IsError {
			t.Fatalf("reply failed: %+v %v", result, err)
		}
		encoded, _ := json.Marshal(result.StructuredContent)
		var value struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(encoded, &value); err != nil || len(value.Data) != 3 || value.Data["status"] != "pending_next_activity" || value.Data["acknowledged"] != false {
			t.Fatalf("reply must return a compact outgoing receipt: %s %v", encoded, err)
		}
		currentID, ok := value.Data["id"].(string)
		if !ok || (replyID != "" && replyID != currentID) {
			t.Fatal("reply retry changed its identity")
		}
		replyID = currentID
	}
	history, err := store.Messages(from.ID, key)
	if err != nil || len(history) != 2 {
		t.Fatalf("reply retry duplicated the outgoing message: %+v %v", history, err)
	}
	var found bool
	for _, msg := range history {
		if msg.ID == replyID && msg.From == to.ID && msg.To == from.ID && msg.ReplyTo == id && msg.Body == "fixture reply" {
			found = true
		}
	}
	if !found {
		t.Fatal("reply lost its exact routing or correlation")
	}
}
