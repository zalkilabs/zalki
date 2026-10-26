package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/zalkilabs/zalki/internal/bus"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCodexVersionGate(t *testing.T) {
	for _, test := range []struct {
		agent string
		ok    bool
	}{
		{"codex/0.164.0", true},
		{"codex/0.164.1 (macos)", true},
		{"codex/0.163.9", false},
		{"codex/0.164.0-rc.1", true},
		{"", false},
		{"codex", false},
	} {
		if codexVersionAtLeast(test.agent, codexToolOutputVersion) != test.ok {
			t.Fatalf("version %q accepted=%v", test.agent, !test.ok)
		}
	}
}

func TestCodexControlSocketAdvertisesDirectDelivery(t *testing.T) {
	root, listener := listenCodexControl(t)
	defer listener.Close()
	target := bus.Session{Kind: "codex", NativeConfigDir: root}
	capabilities := (codexAdapter{}).ReceiveCapabilities(target)
	if capabilities.Delivery != "app_server_unverified_or_queue" || capabilities.BusyDelivery != bus.DeliveryNextTurn || !capabilities.IdleWake {
		t.Fatalf("socket was not advertised: %+v", capabilities)
	}
	var host bus.HealthHost
	health, listenerHealth := (codexAdapter{}).Health(host, target)
	if health != "app_server_socket_present" || listenerHealth != "" {
		t.Fatalf("doctor health = %q %q", health, listenerHealth)
	}
	socketPath := filepath.Join(root, "daemon", "control.sock")
	if err := os.Chmod(socketPath, 0666); err != nil {
		t.Fatal(err)
	}
	if got := (codexAdapter{}).ReceiveCapabilities(target); got.Delivery != "native_queue_next_turn" {
		t.Fatalf("group-accessible socket stayed eligible: %+v", got)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, codexControlDirName), 0755); err != nil {
		t.Fatal(err)
	}
	capabilities = (codexAdapter{}).ReceiveCapabilities(target)
	if capabilities.Delivery != "native_queue_next_turn" || capabilities.BusyDelivery != bus.DeliveryNextTurn {
		t.Fatalf("public control directory stayed eligible: %+v", capabilities)
	}
	health, _ = (codexAdapter{}).Health(host, target)
	if health != "native_queue_only" {
		t.Fatal(health)
	}
}

func TestCodexAppServerDeliveryModes(t *testing.T) {
	for _, mode := range []string{"idle", "busy", "paged", "not-loaded", "identity", "home", "old", "approval", "method-missing", "invalid-params", "internal", "malformed", "approval-rpc", "unsupported", "closed", "hang", "dropped"} {
		t.Run(mode, func(t *testing.T) {
			root, listener := listenCodexControl(t)
			defer listener.Close()
			thread := newTestID()
			body := "deliver this exact body"
			marker := queueSpy(t)
			spec := fakeCodex{Home: root, Thread: thread, Mode: mode}
			calls := serveFakeCodex(t, listener, spec)
			timeout := 8 * time.Second
			if mode == "hang" {
				timeout = 400 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			msg := bus.Message{ID: newTestID(), From: "cursor:" + newTestID(), FromLabel: "Cursor", FromName: "Review", To: "codex:" + thread, Body: body, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			receipt := deliverCodex(ctx, msg, bus.Session{Kind: "codex", ID: msg.To, NativeID: thread, NativeConfigDir: root})
			if strings.Contains(receipt.Output+receipt.Error, body) {
				t.Fatal("receipt exposed the message body")
			}
			queued := queueMarked(t, marker)
			switch mode {
			case "idle", "paged":
				if receipt.Status != bus.StatusSubmitted || receipt.Output != "started_idle_turn" || queued {
					t.Fatalf("status=%s output=%s queued=%v", receipt.Status, receipt.Output, queued)
				}
			case "busy":
				if receipt.Status != bus.StatusSubmitted || receipt.Output != "joined_active_turn" || queued {
					t.Fatalf("status=%s output=%s queued=%v", receipt.Status, receipt.Output, queued)
				}
			case "approval", "approval-rpc":
				if receipt.Status != bus.StatusApprovalBlocked || queued {
					t.Fatalf("status=%s queued=%v", receipt.Status, queued)
				}
			case "hang":
				if receipt.Status != bus.StatusUnknownTimeout || queued {
					t.Fatalf("status=%s queued=%v", receipt.Status, queued)
				}
			case "dropped", "internal", "malformed":
				if receipt.Status != bus.StatusUnknownWrite || queued {
					t.Fatalf("status=%s queued=%v", receipt.Status, queued)
				}
			case "method-missing", "invalid-params":
				if receipt.Status != bus.StatusSubmitted || !queued {
					t.Fatalf("status=%s queued=%v", receipt.Status, queued)
				}
			default:
				if receipt.Status != bus.StatusSubmitted || !queued {
					t.Fatalf("status=%s queued=%v", receipt.Status, queued)
				}
			}
			switch mode {
			case "idle", "paged", "busy":
				verified := (codexAdapter{}).ReceiveCapabilities(bus.Session{Kind: "codex", NativeConfigDir: root})
				if verified.Delivery != "app_server_tool_output" || verified.BusyDelivery != bus.DeliveryBetweenTools {
					t.Fatalf("verified server was not advertised: %+v", verified)
				}
			case "old", "home", "method-missing", "invalid-params":
				unsupported := (codexAdapter{}).ReceiveCapabilities(bus.Session{Kind: "codex", NativeConfigDir: root})
				if unsupported.Delivery != "native_queue_next_turn" || unsupported.BusyDelivery != bus.DeliveryNextTurn {
					t.Fatalf("unusable server was advertised as direct delivery: %+v", unsupported)
				}
			}
			var seen observedCodex
			select {
			case seen = <-calls:
			case <-time.After(8 * time.Second):
				t.Fatal("app-server exchange did not finish")
			}
			if mode == "unsupported" || mode == "closed" {
				if len(seen.methods) != 0 {
					t.Fatalf("transport failure still exchanged methods: %v", seen.methods)
				}
				return
			}
			if len(seen.forbidden) != 0 {
				t.Fatalf("delivery used a session-changing method: %v", seen.forbidden)
			}
			switch mode {
			case "old", "home":
				if strings.Join(seen.methods, ",") != "initialize" {
					t.Fatal(seen.methods)
				}
			case "not-loaded":
				if strings.Join(seen.methods, ",") != "initialize,initialized,thread/loaded/list" {
					t.Fatal(seen.methods)
				}
			case "approval", "identity":
				if strings.Contains(strings.Join(seen.methods, ","), "turn/start") {
					t.Fatal(seen.methods)
				}
			default:
				if !strings.Contains(strings.Join(seen.methods, ","), "turn/start") {
					t.Fatal(seen.methods)
				}
				if seen.turn == nil {
					t.Fatal("missing turn/start")
				}
				for key := range seen.turn {
					switch key {
					case "threadId", "input", "toolOutput":
					default:
						t.Fatalf("turn/start changed session settings via %s", key)
					}
				}
				input, _ := seen.turn["input"].([]any)
				if len(input) != 0 {
					t.Fatal("turn/start included user input")
				}
				tool, _ := seen.turn["toolOutput"].(map[string]any)
				if tool["name"] != "zalki" || tool["namespace"] != "zalki" || tool["retain"] != true || !strings.Contains(tool["output"].(string), body) {
					t.Fatal("tool output lost its origin or body")
				}
			}
		})
	}
}

type fakeCodex struct {
	Home, Thread, Mode string
}

type observedCodex struct {
	methods   []string
	forbidden []string
	turn      map[string]any
}

func listenCodexControl(t *testing.T) (string, net.Listener) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "zalki-codex-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	control := filepath.Join(root, codexControlDirName)
	runtime := filepath.Join(root, "daemon")
	if err := os.MkdirAll(runtime, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(control, 0700); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(runtime, "control.sock")
	listener, err := net.Listen("unix", socketPath)
	if errors.Is(err, syscall.EPERM) {
		t.Skip("execution sandbox blocks Unix socket bind")
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(socketPath, filepath.Join(control, codexSocketName)); err != nil {
		t.Fatal(err)
	}
	return root, listener
}

func queueSpy(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "argv")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + marker + "\"\nprintf '%s\\n' \"$CODEX_HOME\" >> \"" + marker + "\"\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return marker
}

func queueMarked(t *testing.T, marker string) bool {
	t.Helper()
	info, err := os.Stat(marker)
	return err == nil && info.Size() > 0
}

func serveFakeCodex(t *testing.T, listener net.Listener, spec fakeCodex) <-chan observedCodex {
	t.Helper()
	done := make(chan observedCodex, 1)
	if spec.Mode == "closed" {
		listener.Close()
		done <- observedCodex{}
		return done
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		done <- handleFakeCodex(w, r, spec)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { _ = server.Close() })
	return done
}

func handleFakeCodex(w http.ResponseWriter, r *http.Request, spec fakeCodex) observedCodex {
	var seen observedCodex
	if spec.Mode == "unsupported" {
		http.Error(w, "no", http.StatusOK)
		return seen
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return seen
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	home := spec.Home
	if spec.Mode == "home" {
		home = filepath.Join(spec.Home, "other")
	}
	version := "0.164.0"
	if spec.Mode == "old" {
		version = "0.163.0"
	}
	page := 0
	for {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := wsjson.Read(r.Context(), conn, &request); err != nil {
			return seen
		}
		if request.Method == "" {
			continue
		}
		seen.methods = append(seen.methods, request.Method)
		switch request.Method {
		case "thread/resume", "thread/start", "thread/fork", "turn/steer", "turn/interrupt":
			seen.forbidden = append(seen.forbidden, request.Method)
		}
		if len(request.ID) == 0 {
			continue
		}
		if request.Method == "turn/start" {
			seen.turn = request.Params
			if spec.Mode == "dropped" {
				return seen
			}
			if spec.Mode == "hang" {
				time.Sleep(2 * time.Second)
				return seen
			}
		}
		result := fakeCodexResult(spec, request.Method, home, version, &page)
		response := struct {
			ID     json.RawMessage `json:"id"`
			Result any             `json:"result,omitempty"`
			Error  any             `json:"error,omitempty"`
		}{ID: request.ID, Result: result.result, Error: result.failure}
		if err := wsjson.Write(r.Context(), conn, response); err != nil {
			return seen
		}
	}
}

type codexFakeReply struct {
	result  any
	failure any
}

func fakeCodexResult(spec fakeCodex, method, home, version string, page *int) codexFakeReply {
	switch method {
	case "initialize":
		return codexFakeReply{result: map[string]any{"codexHome": home, "platformFamily": "unix", "platformOs": "macos", "userAgent": "codex/" + version}}
	case "thread/loaded/list":
		if spec.Mode == "not-loaded" || spec.Mode == "old" || spec.Mode == "home" {
			return codexFakeReply{result: map[string]any{"data": []string{}}}
		}
		if spec.Mode == "paged" && *page == 0 {
			*page = 1
			cursor := "more"
			return codexFakeReply{result: map[string]any{"data": []string{}, "nextCursor": cursor}}
		}
		return codexFakeReply{result: map[string]any{"data": []string{spec.Thread}}}
	case "thread/read":
		id := spec.Thread
		status := map[string]any{"type": "idle"}
		switch spec.Mode {
		case "busy", "hang", "dropped":
			status = map[string]any{"type": "active", "activeFlags": []string{}}
		case "approval":
			status = map[string]any{"type": "active", "activeFlags": []string{"waitingOnApproval"}}
		case "identity":
			id = newTestID()
		}
		return codexFakeReply{result: map[string]any{"thread": map[string]any{"id": id, "status": status}}}
	case "turn/start":
		switch spec.Mode {
		case "method-missing":
			return codexFakeReply{failure: map[string]any{"code": -32601, "message": "method not found"}}
		case "invalid-params":
			return codexFakeReply{failure: map[string]any{"code": -32602, "message": "invalid params"}}
		case "internal":
			return codexFakeReply{failure: map[string]any{"code": -32603, "message": "internal"}}
		case "malformed":
			return codexFakeReply{failure: "not-an-error-object"}
		case "approval-rpc":
			return codexFakeReply{failure: map[string]any{"code": -32000, "data": map[string]any{"waitingOnApproval": true}}}
		default:
			return codexFakeReply{result: map[string]any{"turn": map[string]any{"id": newTestID(), "items": []any{}, "status": "inProgress"}}}
		}
	default:
		return codexFakeReply{failure: map[string]any{"code": -32601, "message": "method unavailable"}}
	}
}
