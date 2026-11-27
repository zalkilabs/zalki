package mcpserver

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

var _ Bus = (*bus.Service)(nil)

// fakeBus records every call so a test can check which Bus method a tool maps to.
type fakeBus struct {
	calls    []string
	send     bus.SendRequest
	reply    bus.ReplyRequest
	query    bus.MessageQuery
	observed []observedTool
	err      error
}

type observedTool struct {
	name, actor, id string
	err             error
}

// fakeMessageID is a valid UUID: reply, ack and list refuse other message IDs.
const fakeMessageID = "00000000-0000-4000-8000-0000000000a1"

func (f *fakeBus) Send(_ context.Context, r bus.SendRequest) (model.Message, error) {
	f.calls, f.send = append(f.calls, "Send"), r
	return model.Message{ID: "sent-1"}, f.err
}

func (f *fakeBus) Reply(_ context.Context, r bus.ReplyRequest) (model.Message, error) {
	f.calls, f.reply = append(f.calls, "Reply"), r
	return model.Message{ID: "reply-1"}, f.err
}

func (f *fakeBus) Watch(actor, key, to string) (bus.WatchResult, error) {
	f.calls = append(f.calls, "Watch:"+actor+":"+key+":"+to)
	return bus.WatchResult{}, f.err
}

func (f *fakeBus) SetTask(actor, key, task string) (bus.TaskResult, error) {
	f.calls = append(f.calls, "SetTask:"+actor+":"+key+":"+task)
	return bus.TaskResult{}, f.err
}

func (f *fakeBus) ObserveTool(name, actor, id string, _ time.Duration, err error) {
	f.observed = append(f.observed, observedTool{name, actor, id, err})
}

func (f *fakeBus) RuntimeStatus() bus.RuntimeStatus { return bus.RuntimeStatus{} }

func (f *fakeBus) UncertainDelivery(kind string) (string, string) { return codexNotes(kind) }

func (f *fakeBus) Ack(actor, key, id string) (model.Message, error) {
	f.calls = append(f.calls, "Ack:"+actor+":"+key+":"+id)
	return model.Message{ID: id}, f.err
}

func (f *fakeBus) ReadMessages(q bus.MessageQuery) (bus.MessageBatch, error) {
	f.calls, f.query = append(f.calls, "ReadMessages"), q
	return bus.MessageBatch{}, f.err
}

func (f *fakeBus) DiscoverWithWarnings(actor, key string) ([]bus.AgentInfo, int, error) {
	f.calls = append(f.calls, "DiscoverWithWarnings:"+actor+":"+key)
	return nil, 0, f.err
}

func fakeClient(t *testing.T, fake Bus) (*mcp.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	ct, st := mcp.NewInMemoryTransports()
	server, err := newServer(fake).Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "fake", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client, ctx
}

func TestEachToolCallsItsBusMethodAndObservesIt(t *testing.T) {
	fake := &fakeBus{}
	client, ctx := fakeClient(t, fake)
	cred := map[string]any{"actor": "a", "capability": "k"}
	with := func(extra map[string]any) map[string]any {
		out := map[string]any{}
		for k, v := range cred {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	tools := []struct {
		name string
		args map[string]any
		want string
	}{
		{"list_agents", cred, "DiscoverWithWarnings:a:k"},
		{"send_message", with(map[string]any{"to": "t", "body": "b", "message_id": "m"}), "Send"},
		{"reply_message", with(map[string]any{"message_id": fakeMessageID, "body": "b"}), "Reply"},
		{"ack_message", with(map[string]any{"message_id": fakeMessageID}), "Ack:a:k:" + fakeMessageID},
		{"list_messages", cred, "ReadMessages"},
		{"watch_agent", with(map[string]any{"to": "t"}), "Watch:a:k:t"},
		{"set_task", with(map[string]any{"task": "x"}), "SetTask:a:k:x"},
	}
	for _, tool := range tools {
		before := len(fake.calls)
		if _, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tool.name, Arguments: tool.args}); err != nil {
			t.Fatalf("%s: %v", tool.name, err)
		}
		if got := fake.calls[before:]; len(got) != 1 || got[0] != tool.want {
			t.Fatalf("%s called %v, want [%s]", tool.name, got, tool.want)
		}
	}
	names := make([]string, 0, len(fake.observed))
	for _, o := range fake.observed {
		names = append(names, o.name)
		if o.actor != "a" || o.err != nil {
			t.Fatalf("observed %+v", o)
		}
	}
	want := []string{"list_agents", "send_message", "reply_message", "ack_message", "list_messages", "watch_agent", "set_task"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("observed tools %v, want %v", names, want)
	}
	if fake.send != (bus.SendRequest{Actor: "a", Capability: "k", To: "t", MessageID: "m", Body: "b"}) {
		t.Fatalf("send request %+v", fake.send)
	}
	if fake.reply != (bus.ReplyRequest{Actor: "a", Capability: "k", MessageID: fakeMessageID, Body: "b"}) {
		t.Fatalf("reply request %+v", fake.reply)
	}
	if fake.query.Actor != "a" || fake.query.Capability != "k" {
		t.Fatalf("query %+v", fake.query)
	}
}

func TestToolErrorReachesObserveToolAndOutput(t *testing.T) {
	boom := errors.New("boom")
	fake := &fakeBus{err: boom}
	client, ctx := fakeClient(t, fake)
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "ack_message", Arguments: map[string]any{"actor": "a", "capability": "k", "message_id": fakeMessageID}})
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("a failed call must be an error result")
	}
	if len(fake.observed) != 1 || !errors.Is(fake.observed[0].err, boom) {
		t.Fatalf("observed %+v", fake.observed)
	}
}
