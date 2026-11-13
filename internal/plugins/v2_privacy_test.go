package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// pluginMail sends a private plugin message to the asker, then installs a
// second plugin with a valid Read grant for the asker.
func (x *v2Fixture) pluginMail(t *testing.T) (model.Message, pluginfeed.PluginFeed, pluginfeed.PluginFeedStatus) {
	t.Helper()
	service := bus.Service{Store: x.store, PluginSend: pluginfeed.BoundPluginSend(&x.status)}
	m, err := service.SendFromPlugin(context.Background(), bus.PluginSendRequest{Installation: x.status.ID, Name: x.status.Name, ClientMessageID: "private", ReplyTo: x.request.ID, To: x.asker.ID, Body: "PRIVATE_SEND_BODY"})
	if err != nil {
		t.Fatal(err)
	}
	reader := pluginfeed.NewPluginFeed(x.store, "reader")
	if err := reader.AddExternalPlugin(x.binary, nil); err != nil {
		t.Fatal(err)
	}
	if err := reader.ConfigurePluginFeed([]string{x.asker.ID}); err != nil {
		t.Fatal(err)
	}
	if err := reader.GrantPluginCapabilities(pluginfeed.PluginCapabilityGrants{Read: []string{x.asker.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := reader.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := reader.PluginStatus()
	if err != nil || status.ID == x.status.ID {
		t.Fatalf("reader status: %+v %v", status, err)
	}
	return m, reader, status
}

func TestV2ReadRefusesAnotherPluginsMail(t *testing.T) {
	x := v2HostFixture(t)
	m, reader, status := x.pluginMail(t)
	h := &v2Host{feed: reader, binding: &status}
	r := &v2Result{Op: "read", RequestID: "r"}
	if h.read(&v2Read{Session: x.asker.ID, Message: m.ID}, r); r.Code != "unavailable" || r.Result != nil {
		t.Fatalf("v2 read gave another plugin's mail: %+v", r)
	}
}

// inflate rewrites a stored inbox message with body and fixes its index
// entry, the way base c0d7472 admitted a body before the frame-size check.
func (x *v2Fixture) inflate(t *testing.T, id, body string) {
	t.Helper()
	size := rewriteJSON(t, filepath.Join(x.store.Dir, "messages", id+".json"), func(v map[string]any) { v["body"] = body })
	rewriteJSON(t, filepath.Join(x.status.DataDir, "inbox.json"), func(v map[string]any) {
		for _, e := range v["entries"].([]any) {
			if entry := e.(map[string]any); entry["id"] == id {
				entry["encoded_message_bytes"] = size
			}
		}
	})
}

func rewriteJSON(t *testing.T, path string, change func(map[string]any)) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var v map[string]any
	if err := decoder.Decode(&v); err != nil {
		t.Fatal(err)
	}
	change(v)
	if raw, err = json.Marshal(v); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return len(raw)
}

// An old record whose body is too large for one frame comes back from
// inbox_next as too_large without its body. The child is not stopped, the
// record stays until the child consumes it, and consume works.
func TestV2HostWithholdsAnOldOversizedBody(t *testing.T) {
	x := v2HostFixture(t)
	x.inflate(t, x.request.ID, strings.Repeat("<", 16<<10))
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	defer wr.Close()
	h := &v2Host{ctx: t.Context(), feed: x.feed, binding: &x.status, child: &child{input: wr}}
	in := bufio.NewReader(rd)
	for range 2 {
		line := x.handleLine(t, h, in, &v2Command{Op: "inbox_next", RequestID: "n"})
		var reply struct {
			Reply struct {
				Code   string `json:"code"`
				Result struct {
					Message map[string]any `json:"message"`
				} `json:"result"`
			} `json:"reply"`
		}
		if json.Unmarshal(line, &reply) != nil || len(line) > 1024 || reply.Reply.Code != "too_large" {
			t.Fatalf("inbox_next of an oversized body: %s", line)
		}
		if m := reply.Reply.Result.Message; m["id"] != x.request.ID || m["body_bytes"] != float64(16<<10) || m["body"] != nil {
			t.Fatalf("too_large result: %v", m)
		}
	}
	if x.record(t, x.request.ID).AcknowledgedAt != "" {
		t.Fatal("inbox_next acknowledged the message")
	}
	line := x.handleLine(t, h, in, &v2Command{Op: "inbox_consume", RequestID: "c", Consume: &v2Consume{Message: x.request.ID}})
	if !bytes.Contains(line, []byte(`"code":"ok"`)) || x.record(t, x.request.ID).AcknowledgedAt == "" {
		t.Fatalf("consume of the oversized message: %s", line)
	}
}

// handleLine runs one operation and returns the host's reply frame.
func (x *v2Fixture) handleLine(t *testing.T, h *v2Host, in *bufio.Reader, f *v2Command) []byte {
	t.Helper()
	if err := h.handle(f); err != nil {
		t.Fatalf("the host stopped the child: %v", err)
	}
	line, err := in.ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	return line
}
