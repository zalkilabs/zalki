package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestStdioRecoveryServerProcess(t *testing.T) {
	state := os.Getenv("ZALKI_STDIO_RECOVERY_STATE")
	if state == "" {
		t.Skip("subprocess fixture")
	}
	service := &bus.Service{Store: bus.Store{Dir: state, Registry: adapters.Builtin()}}
	if err := Run(context.Background(), service, os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type recoveryRPC struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	readErr error
}

type recoveryStderr struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *recoveryStderr) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *recoveryStderr) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

type recoveryProcess struct {
	input  io.WriteCloser
	output io.ReadCloser
	frames chan recoveryRPC
	stderr *recoveryStderr
}

func startRecoveryProcess(t *testing.T, state string) *recoveryProcess {
	t.Helper()
	proc := startRecoveryPipes(t, state)
	go proc.collect(proc.output)
	return proc
}

func startRecoveryPipes(t *testing.T, state string) *recoveryProcess {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStdioRecoveryServerProcess$")
	if binary := os.Getenv("ZALKI_TEST_BINARY"); binary != "" {
		cmd = exec.CommandContext(ctx, binary, "--state", state, "--log-level", "off", "mcp")
	}
	cmd.Env = append(os.Environ(), "ZALKI_STDIO_RECOVERY_STATE="+state, "CODEX_HOME="+t.TempDir(), "CLAUDE_CONFIG_DIR="+t.TempDir(), "CURSOR_CONFIG_DIR="+t.TempDir())
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	proc := &recoveryProcess{input: input, output: output, frames: make(chan recoveryRPC, 128), stderr: &recoveryStderr{}}
	cmd.Stderr = proc.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = input.Close(); _ = cmd.Wait(); cancel() })
	return proc
}

func (p *recoveryProcess) collect(output io.Reader) {
	defer close(p.frames)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		if !validStdioJSON(scanner.Bytes()) {
			p.frames <- recoveryRPC{readErr: errors.New("invalid UTF-8 JSON on stdout")}
			return
		}
		var frame recoveryRPC
		frame.readErr = json.Unmarshal(scanner.Bytes(), &frame)
		p.frames <- frame
	}
	if err := scanner.Err(); err != nil {
		p.frames <- recoveryRPC{readErr: err}
	}
}

func (p *recoveryProcess) read(t *testing.T) recoveryRPC {
	t.Helper()
	select {
	case frame, ok := <-p.frames:
		if !ok || frame.readErr != nil {
			t.Fatalf("stdio server ended or emitted invalid JSON: %v; stderr=%s", frame.readErr, p.stderr.String())
		}
		return frame
	case <-time.After(10 * time.Second):
		t.Fatalf("stdio response timed out; stderr=%s", p.stderr.String())
		return recoveryRPC{}
	}
}

func (p *recoveryProcess) write(t *testing.T, data string) {
	t.Helper()
	if _, err := io.WriteString(p.input, data); err != nil {
		t.Fatal("stdio input failed", err)
	}
}

func (p *recoveryProcess) initialize(t *testing.T) {
	t.Helper()
	p.write(t, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-11-25\",\"capabilities\":{},\"clientInfo\":{\"name\":\"recovery-fixture\",\"version\":\"0\"}}}\n")
	if frame := p.read(t); string(frame.ID) != "1" || frame.Error != nil {
		t.Fatal("stdio initialization failed")
	}
	p.write(t, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n")
}

func assertRecoveryCalls(t *testing.T, p *recoveryProcess, store bus.Store) {
	t.Helper()
	from, key, to, _ := registerPair(t, store, "cursor", "00000000-0000-4000-8000-000000000091", "cursor", "00000000-0000-4000-8000-000000000092")
	p.write(t, "{\"jsonrpc\":\"2.0\",\"id\":10,\"method\":\"tools/list\"}\r\n")
	frame := p.read(t)
	var listed struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if frame.Error != nil || string(frame.ID) != "10" || json.Unmarshal(frame.Result, &listed) != nil || len(listed.Tools) != 7 {
		t.Fatal("tools/list did not recover on the same process")
	}
	request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 11, "method": "tools/call", "params": map[string]any{"name": "send_message", "arguments": map[string]any{"actor": from.ID, "capability": key, "to": to.ID, "body": "recovered send"}}})
	if err != nil {
		t.Fatal(err)
	}
	p.write(t, string(request)+"\n")
	frame = p.read(t)
	var sent struct {
		IsError    bool `json:"isError"`
		Structured struct {
			Data MessageReceipt `json:"data"`
		} `json:"structuredContent"`
	}
	if frame.Error != nil || string(frame.ID) != "11" || json.Unmarshal(frame.Result, &sent) != nil || sent.IsError || sent.Structured.Data.ID == "" {
		t.Fatal("send_message did not recover on the same process")
	}
}

func TestSTDIORecoversBadFramesWithoutLosingTools(t *testing.T) {
	for _, test := range []struct {
		name  string
		bad   string
		codes []int
		id    string
	}{
		{"oversize", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"send_message","arguments":{"body":"` + strings.Repeat("x", 140000) + "\"}}}\n", []int{-32600}, "2"},
		{"ID after other fields", `{"jsonrpc":"2.0","params":{"id":"nested"},"id":"right","method":"tools/call","padding":"` + strings.Repeat("x", 140000) + "\"}\n", []int{-32600}, `"right"`},
		{"ID beyond prefix", `{"jsonrpc":"2.0","padding":"` + strings.Repeat("x", 140000) + "\",\"id\":3,\"method\":\"tools/call\"}\n", []int{-32600}, "null"},
		{"nested ID ignored", `{"jsonrpc":"2.0","method":"tools/call","params":{"id":"nested","body":"` + strings.Repeat("x", 140000) + "\"}}\n", []int{-32600}, "null"},
		{"duplicate ID", `{"jsonrpc":"2.0","id":2,"id":3,"method":"tools/call","params":{"body":"` + strings.Repeat("x", 140000) + "\"}}\n", []int{-32600}, "null"},
		{"malformed", "{private-invalid-json\n", []int{-32700}, "null"},
		{"two bad CRLF frames", "bad\r\n{bad-again\r\n", []int{-32700, -32700}, "null"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			p := startRecoveryProcess(t, store.Dir)
			p.initialize(t)
			for _, part := range []string{test.bad[:1], test.bad[1:]} {
				p.write(t, part)
			}
			for _, code := range test.codes {
				frame := p.read(t)
				if string(frame.ID) != test.id || frame.Error == nil || frame.Error.Code != code || strings.Contains(frame.Error.Message, "private") {
					t.Fatal("expected JSON-RPC frame error missing")
				}
			}
			p.write(t, "{\"jsonrpc\":\"2.0\",\"method\":\"notifications/cancelled\",\"params\":{\"requestId\":999}}\r\n")
			assertRecoveryCalls(t, p, store)
		})
	}
}

func TestSTDIOTrailingJSONWhitespaceKeepsToolsAvailable(t *testing.T) {
	store := newTestStore(t)
	p := startRecoveryProcess(t, store.Dir)
	p.initialize(t)
	for i, trailing := range []string{" \n", "\t\r\n"} {
		id := strconv.Itoa(i + 20)
		p.write(t, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"ping"}%s`, id, trailing))
		if frame := p.read(t); string(frame.ID) != id || frame.Error != nil {
			t.Fatal("valid ping with trailing JSON whitespace was rejected")
		}
	}
	assertRecoveryCalls(t, p, store)
}

func TestSTDIOOversizeStringIDsKeepRecoveryFramesValid(t *testing.T) {
	const head = `{"jsonrpc":"2.0","id":%s,"method":"ping","padding":"`
	const validID = `"שלום🚦"`
	cutRune := strings.Repeat("x", maxStdioIDPrefix-len(fmt.Sprintf(head, validID))-1) + "🙂"
	for _, test := range []struct{ name, id, padding, want string }{
		{"invalid UTF-8 ID", "\"bad\xff\"", "", "null"},
		{"literal C1 ID", "\"a\u0085b\"", "", "\"a\u0085b\""},
		{"literal DEL ID", "\"a\x7fb\"", "", "\"a\x7fb\""},
		{"literal line separator ID", "\"a\u2028b\"", "", "\"a\u2028b\""},
		{"escaped NUL ID", `"a\u0000b"`, "", `"a\u0000b"`},
		{"escaped quote ID", `"a\"b"`, "", `"a\"b"`},
		{"multibyte ID", validID, "", validID},
		{"cut rune after ID", validID, cutRune, validID},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newTestStore(t)
			p := startRecoveryProcess(t, store.Dir)
			p.initialize(t)
			p.write(t, fmt.Sprintf(head, test.id)+test.padding+strings.Repeat("x", maxStdioFrameBytes)+"\"}\n")
			if frame := p.read(t); string(frame.ID) != test.want || frame.Error == nil || frame.Error.Code != -32600 {
				t.Fatal("unexpected oversized string ID response")
			}
			assertRecoveryCalls(t, p, store)
		})
	}
}

type lateIDWriter struct{ io.WriteCloser }

func (w lateIDWriter) Write(p []byte) (int, error) {
	if len(p) <= maxStdioFrameBytes {
		return w.WriteCloser.Write(p)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(p, &fields); err != nil {
		return 0, err
	}
	frame := `{"jsonrpc":"2.0","method":` + string(fields["method"]) + `,"params":` + string(fields["params"]) + `,"id":` + string(fields["id"]) + "}\n"
	if _, err := io.WriteString(w.WriteCloser, frame); err != nil {
		return 0, err
	}
	return len(p), nil
}

func TestSTDIOSDKClientCorrelatedOversizeAndNullFallback(t *testing.T) {
	for _, lateID := range []bool{false, true} {
		t.Run(fmt.Sprintf("late-id=%v", lateID), func(t *testing.T) {
			store := newTestStore(t)
			p := startRecoveryPipes(t, store.Dir)
			writer := p.input
			if lateID {
				writer = lateIDWriter{p.input}
			}
			session, err := mcp.NewClient(&mcp.Implementation{Name: "SDK-recovery", Version: "0"}, nil).Connect(t.Context(), &mcp.IOTransport{Reader: p.output, Writer: writer}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = session.Close() })
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			_, callErr := session.CallTool(ctx, &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"body": strings.Repeat("x", 140000)}})
			if callErr == nil {
				t.Fatal("oversized call unexpectedly succeeded")
			}
			_, nextErr := session.ListTools(t.Context(), nil)
			t.Logf("SDK observation lateID=%v: call=%v deadline=%v next-list=%v", lateID, callErr, errors.Is(callErr, context.DeadlineExceeded), nextErr)
			if !lateID && (errors.Is(callErr, context.DeadlineExceeded) || !strings.Contains(callErr.Error(), "request too large; limit 128 KiB") || nextErr != nil) {
				t.Fatal("correlated error lost the SDK client's tools")
			}
			if !lateID {
				assertSDKRecoveredSend(t, session, store)
			}
		})
	}
}

func assertSDKRecoveredSend(t *testing.T, session *mcp.ClientSession, store bus.Store) {
	t.Helper()
	from, key, to, _ := registerPair(t, store, "cursor", "00000000-0000-4000-8000-000000000093", "cursor", "00000000-0000-4000-8000-000000000094")
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_message", Arguments: map[string]any{"actor": from.ID, "capability": key, "to": to.ID, "body": "SDK remains connected"}})
	if err != nil || result.IsError {
		t.Fatal("send_message failed after correlated frame error", err)
	}
}

func TestSTDIORecoveryErrorsDoNotInterleaveConcurrentResponses(t *testing.T) {
	p := startRecoveryProcess(t, newTestStore(t).Dir)
	p.initialize(t)
	for i := range 20 {
		p.write(t, fmt.Sprintf("{\"jsonrpc\":\"2.0\",\"id\":%d,\"method\":\"ping\"}\nbad-json\n", i+100))
	}
	seen, bad := map[string]bool{}, 0
	for range 40 {
		frame := p.read(t)
		switch id := string(frame.ID); {
		case id == "null" && frame.Error != nil && frame.Error.Code == -32700:
			bad++
		case frame.Error == nil && !seen[id]:
			seen[id] = true
		default:
			t.Fatal("corrupted or duplicate concurrent response")
		}
	}
	if bad != 20 || len(seen) != 20 {
		t.Fatal("a recovery error or normal response disappeared")
	}
}
