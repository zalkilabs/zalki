package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestMCPSignalInterruptedSend(t *testing.T) {
	for _, sig := range []os.Signal{syscall.SIGTERM, os.Interrupt, syscall.SIGHUP} {
		t.Run(sig.String(), func(t *testing.T) { runMCPSignalInterruptedSend(t, sig) })
	}
}

func runMCPSignalInterruptedSend(t *testing.T, sig os.Signal) {
	t.Helper()
	state := filepath.Join(t.TempDir(), "state")
	marker := filepath.Join(t.TempDir(), "mid-write")
	store := bus.Store{Dir: state, Registry: signalTestRegistry(marker)}
	actor, key, err := store.Register("signaltest", "00000000-0000-4000-8000-000000000101", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := store.Register("signaltest", "00000000-0000-4000-8000-000000000102", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPSignalProcessHelper$")
	cmd.Env = append(os.Environ(), "ZALKI_SIGNAL_PROCESS_HELPER=1", "ZALKI_SIGNAL_STATE="+state, "ZALKI_SIGNAL_MARKER="+marker)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	encoder := json.NewEncoder(input)
	initializeSignalMCP(t, encoder, output)
	const id = "00000000-0000-4000-8000-000000000103"
	request := bus.SendRequest{Actor: actor.ID, Capability: key, To: target.ID, MessageID: id, Body: "private signal send fixture"}
	args := map[string]any{"actor": request.Actor, "capability": request.Capability, "to": request.To, "message_id": id, "body": request.Body}
	if err = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "send_message", "arguments": args}}); err != nil {
		t.Fatal(err)
	}
	waitSignalWrite(t, marker)
	assertSignalSendAttempting(t, store, request.MessageID)
	if err = cmd.Process.Signal(sig); err != nil {
		t.Fatal(err)
	}
	err = cmd.Wait()
	if err != nil || cmd.ProcessState.ExitCode() != 0 {
		t.Fatalf("signal result: %v; %s", err, diagnostic.String())
	}
	if strings.Contains(diagnostic.String(), `"level":"ERROR"`) || strings.Contains(diagnostic.String(), "zalki: context canceled") || !strings.Contains(diagnostic.String(), `"msg":"mcp.stopped"`) {
		t.Fatal("unclean signal diagnostics", diagnostic.String())
	}
	assertSignalSendSettled(t, store, request)
}

func initializeSignalMCP(t *testing.T, encoder *json.Encoder, output io.Reader) {
	t.Helper()
	params := map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "signal-fixture", "version": "1"}}
	if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": params}); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(output).ReadBytes('\n')
	var response struct {
		ID int `json:"id"`
	}
	if err != nil || json.Unmarshal(line, &response) != nil || response.ID != 1 {
		t.Fatal("initialize failed", string(line), err)
	}
	if err = encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		t.Fatal(err)
	}
}

func waitSignalWrite(t *testing.T, marker string) {
	t.Helper()
	limit := time.Now().Add(3 * time.Second)
	for time.Now().Before(limit) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("send did not reach a blocked partial write")
}

func assertSignalSendSettled(t *testing.T, store bus.Store, request bus.SendRequest) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(store.Dir, "messages", request.MessageID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved model.Message
	if err = json.Unmarshal(raw, &saved); err != nil {
		t.Fatal("torn message record", err)
	}
	if saved.ID != request.MessageID || saved.Receipt == nil || saved.Status != model.StatusUnknownWrite {
		t.Fatalf("send not durably settled: %+v", saved)
	}
	service := bus.Service{Store: store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		t.Fatal("same-ID retry attempted another write")
		return model.NewReceipt(model.StatusNotSent)
	}}
	retry, err := service.Send(t.Context(), request)
	if err != nil || retry.ID != saved.ID || retry.Status != saved.Status {
		t.Fatalf("same-ID retry: %+v %v", retry, err)
	}
	t.Logf("mid-write send settled: status=%s receipt=%s same-ID retry preserved", saved.Status, saved.Receipt.Status)
}

func assertSignalSendAttempting(t *testing.T, store bus.Store, id string) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(store.Dir, "messages", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var message model.Message
	if err = json.Unmarshal(raw, &message); err != nil {
		t.Fatal(err)
	}
	if message.Status != model.StatusAttempting || message.Receipt != nil {
		t.Fatal("fixture write finished before signal", message.Status)
	}
}
