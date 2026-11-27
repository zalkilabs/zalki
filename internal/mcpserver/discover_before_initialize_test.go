package mcpserver

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// Antigravity (agy 1.5.18) sends the method "server/discover" before "initialize".
// That is not an MCP method of this server. The server must answer it with a
// JSON-RPC error, must not crash or hang, and must serve the normal start that
// follows. The request is the shape agy sends: params hold only a _meta.

type rpcLine struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code int `json:"code"`
	} `json:"error"`
}

// rpcPeer is the client side of a raw stdio conversation with Run.
type rpcPeer struct {
	t        *testing.T
	toServer *io.PipeWriter
	lines    chan string
}

func startRawServer(t *testing.T) (*rpcPeer, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	toServerR, toServerW := io.Pipe()
	fromServerR, fromServerW := io.Pipe()
	done := make(chan error, 1)
	service := newTestService(t)
	go func() { done <- Run(ctx, service, toServerR, fromServerW) }()
	peer := &rpcPeer{t: t, toServer: toServerW, lines: make(chan string, 16)}
	go func() {
		scanner := bufio.NewScanner(fromServerR)
		for scanner.Scan() {
			peer.lines <- scanner.Text()
		}
		close(peer.lines)
	}()
	return peer, done
}

func (p *rpcPeer) send(line string) {
	p.t.Helper()
	if _, err := io.WriteString(p.toServer, line+"\n"); err != nil {
		p.t.Fatal(err)
	}
}

// answer reads the next line, which must answer request id.
func (p *rpcPeer) answer(id string) rpcLine {
	p.t.Helper()
	select {
	case line, ok := <-p.lines:
		if !ok {
			p.t.Fatalf("the server closed its output before it answered request %s", id)
		}
		var got rpcLine
		if err := json.Unmarshal([]byte(line), &got); err != nil || strings.TrimSpace(string(got.ID)) != id {
			p.t.Fatalf("answer %q (%v); want one for request %s", line, err, id)
		}
		return got
	case <-time.After(10 * time.Second):
		p.t.Fatalf("no answer to request %s: the server hangs", id)
	}
	return rpcLine{}
}

func TestAMethodBeforeInitializeGetsAnErrorAndTheServerKeepsServing(t *testing.T) {
	peer, done := startRawServer(t)
	peer.send(`{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/clientInfo":{"name":"agy","version":"1"}}}}`)
	if got := peer.answer("1"); got.Error == nil || got.Result != nil {
		t.Fatalf("server/discover before initialize: %+v; want a JSON-RPC error", got)
	}
	peer.send(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"agy","version":"1"}}}`)
	if got := peer.answer("2"); got.Error != nil || len(got.Result) == 0 {
		t.Fatalf("initialize after the unknown method: %+v", got)
	}
	peer.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	peer.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if list := peer.answer("3"); list.Error != nil || !strings.Contains(string(list.Result), `"send_message"`) {
		t.Fatalf("tools/list after the unknown method: %+v", list)
	}
	// The same unknown method after the start is an error as well.
	peer.send(`{"jsonrpc":"2.0","id":4,"method":"server/discover","params":{}}`)
	if got := peer.answer("4"); got.Error == nil {
		t.Fatalf("server/discover after initialize: %+v; want a JSON-RPC error", got)
	}
	_ = peer.toServer.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the server did not stop after its input closed")
	}
}
