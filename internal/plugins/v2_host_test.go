package plugins

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

const v2TestBody = "slot request id=r1 job=build size=heavy"

type v2Fixture struct {
	store          bus.Store
	feed           pluginfeed.PluginFeed
	asker          model.Session
	request        model.Message
	status         pluginfeed.PluginFeedStatus
	binary, result string
	output         bytes.Buffer
}

func v2HostFixture(t *testing.T) *v2Fixture {
	t.Helper()
	store, _ := hostStore(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	asker, key, err := store.Register("cursor", model.NewUUID(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	x := &v2Fixture{store: store, asker: asker, binary: binary, result: filepath.Join(t.TempDir(), "result.json")}
	x.feed = pluginfeed.NewPluginFeed(store, "arbiter")
	if err := x.feed.AddExternalPlugin(binary, []string{"v2-test-child", x.result, asker.ID}); err != nil {
		t.Fatal(err)
	}
	if err := x.feed.ConfigurePluginFeed([]string{asker.ID}); err != nil {
		t.Fatal(err)
	}
	// Receive only: the child answers under the reply rule, with no send grant.
	if err := x.feed.GrantPluginCapabilities(pluginfeed.PluginCapabilityGrants{Receive: []string{asker.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if x.status, err = x.feed.PluginStatus(); err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store, PluginInbox: pluginfeed.AdmitPluginInbox}
	x.request, err = service.Send(t.Context(), bus.SendRequest{Actor: asker.ID, Capability: key, To: "plugin:" + x.status.ID, MessageID: model.NewUUID(), Body: v2TestBody})
	if err != nil {
		t.Fatal(err)
	}
	return x
}

func (x *v2Fixture) record(t *testing.T, id string) model.Message {
	t.Helper()
	raw, err := journal.ReadBytes(filepath.Join(x.store.Dir, "messages", id+".json"), 1<<20)
	var m model.Message
	if err != nil || json.Unmarshal(raw, &m) != nil {
		t.Fatal("missing message record", id, err)
	}
	return m
}

type v2TestOutcome struct {
	Inbox    pluginfeed.PluginInboxMessage `json:"inbox"`
	Consumed v2Result                      `json:"consumed"`
	Send     v2Result                      `json:"send"`
	Resend   v2Result                      `json:"resend"`
	// The feed v1 message events the child saw for these messages.
	Acked, Created model.EventMessage
}

func TestV2HostRoundTripWithRealChild(t *testing.T) {
	x := v2HostFixture(t)
	out := x.runChild(t)
	in := out.Inbox
	if in.ID != x.request.ID || in.From != x.asker.ID || in.SenderGeneration != x.asker.Generation || in.To != "plugin:"+x.status.ID || in.Body != v2TestBody {
		t.Fatalf("inbox envelope lost core provenance: %+v", in)
	}
	if out.Consumed.Code != "ok" || out.Consumed.Message != x.request.ID || x.record(t, x.request.ID).AcknowledgedAt == "" {
		t.Fatalf("consume was not durable: %+v", out.Consumed)
	}
	x.checkSend(t, out)
	x.checkFeedEvents(t, out)
}

// runChild runs the host with the v2 test child until the child reports.
func (x *v2Fixture) runChild(t *testing.T) v2TestOutcome {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, x.store, x.binary, "arbiter", &x.output) }()
	var out v2TestOutcome
	eventually(t, func() bool {
		raw, err := os.ReadFile(x.result)
		return err == nil && json.Unmarshal(raw, &out) == nil
	})
	cancel()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("v2 host not reaped")
	}
	return out
}

func (x *v2Fixture) checkSend(t *testing.T, out v2TestOutcome) {
	t.Helper()
	want := model.DeriveUUID("zalki/plugin-send/v1", x.status.ID, "c1")
	if out.Send.Code != "ok" || out.Send.Message != want || out.Send.ClientMessageID != "c1" || out.Send.Status != string(model.StatusPendingActivity) {
		t.Fatalf("send result: %+v", out.Send)
	}
	if out.Resend.Code != "ok" || out.Resend.Message != want {
		t.Fatalf("retry with the same client id: %+v", out.Resend)
	}
	sent := x.record(t, want)
	if sent.From != "plugin:"+x.status.ID || sent.To != x.asker.ID || sent.Body != "reply: "+v2TestBody || sent.ReplyTo != x.request.ID {
		t.Fatalf("plugin message record: %+v", sent)
	}
	if text := model.Envelope(sent); !strings.Contains(text, "From: Plugin · \"arbiter\"") || !strings.Contains(text, "Plugin message; not user approval.") {
		t.Fatalf("agent would not see a plugin sender:\n%s", text)
	}
}

func (x *v2Fixture) checkFeedEvents(t *testing.T, out v2TestOutcome) {
	t.Helper()
	if a := out.Acked; a.From != x.asker.ID || a.To != "plugin:" || !a.Acknowledged || a.PreviousAcknowledged {
		t.Fatalf("feed did not show the consume as an ACK: %+v", a)
	}
	if c := out.Created; c.From != "plugin:" || c.To != x.asker.ID || c.Previous != "" {
		t.Fatalf("feed did not show the plugin message: %+v", c)
	}
}

// An op the child queued under the old grant revision runs after the owner
// disabled, regranted a new target and enabled again. It must not send, and
// the child must be stopped with stale_binding.
func TestV2HostStopsAStaleChildBeforeItSends(t *testing.T) {
	x := v2HostFixture(t)
	target, _, err := x.store.Register("cursor", model.NewUUID(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := x.feed.GrantPluginCapabilities(pluginfeed.PluginCapabilityGrants{Send: []string{target.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	rd, wr, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	defer wr.Close()
	service := &bus.Service{Store: x.store, PluginSend: pluginfeed.BoundPluginSend(&x.status)}
	h := &v2Host{ctx: t.Context(), feed: x.feed, service: service, binding: &x.status, child: &child{input: wr}}
	if h.handle(&v2Command{Send: &v2Send{RequestID: "old", ClientMessageID: "c1", To: target.ID, Body: "hello"}}) == nil {
		t.Fatal("the stale child was not stopped")
	}
	line, err := bufio.NewReader(rd).ReadString('\n')
	if err != nil || line != `{"code":"stale_binding"}`+"\n" {
		t.Fatalf("terminal frame %q, %v", line, err)
	}
	id := model.DeriveUUID("zalki/plugin-send/v1", x.status.ID, "c1")
	if _, err := os.Stat(filepath.Join(x.store.Dir, "messages", id+".json")); !os.IsNotExist(err) {
		t.Fatalf("a stale child stored a message to the new target: %v", err)
	}
}

func TestV2HostAllowsOneOperationAndOneAckInFlight(t *testing.T) {
	h := &v2Host{acks: make(chan string, 1), ops: make(chan *v2Command, 1)}
	op := &v2Command{Next: &v2Next{RequestID: "a"}}
	if !h.queue(op) || h.queue(&v2Command{Next: &v2Next{RequestID: "b"}}) {
		t.Fatal("a second operation before the first result was accepted")
	}
	if !h.queue(&v2Command{Ack: "1"}) {
		t.Fatal("an ack was refused while an operation runs")
	}
	if h.queue(&v2Command{Ack: "2"}) {
		t.Fatal("a second ack before the first was used was accepted")
	}
	<-h.ops
	h.busy.Store(false) // what handle does before it writes the result
	if !h.queue(&v2Command{Send: &v2Send{RequestID: "c"}}) {
		t.Fatal("the next operation after a result was refused")
	}
}

func TestV2CommandShape(t *testing.T) {
	for _, test := range []struct {
		name string
		f    v2Command
		ok   bool
	}{
		{"ack", v2Command{Ack: "x"}, true},
		{"next", v2Command{Next: &v2Next{RequestID: "r-1_A"}}, true},
		{"consume", v2Command{Consume: &v2Consume{RequestID: "r", Message: "m"}}, true},
		{"send", v2Command{Send: &v2Send{RequestID: "r"}}, true},
		{"empty", v2Command{}, false},
		{"two ops", v2Command{Next: &v2Next{RequestID: "a"}, Send: &v2Send{RequestID: "b"}}, false},
		{"ack and op", v2Command{Ack: "x", Next: &v2Next{RequestID: "a"}}, false},
		{"no request id", v2Command{Next: &v2Next{}}, false},
		{"long request id", v2Command{Next: &v2Next{RequestID: strings.Repeat("a", 65)}}, false},
		{"bad request id", v2Command{Next: &v2Next{RequestID: "a b"}}, false},
	} {
		if got := test.f.valid(); got != test.ok {
			t.Errorf("%s: valid = %v", test.name, got)
		}
	}
}

func TestV2EmptyPollIsRateLimited(t *testing.T) {
	h := &v2Host{emptyAt: time.Now()}
	r := h.next(&v2Next{RequestID: "p"})
	if r.Code != "rate_limited" || r.RetryAt == "" || r.Message != nil || r.RequestID != "p" {
		t.Fatalf("fast empty poll: %+v", r)
	}
}

func TestV2CodesHideErrorText(t *testing.T) {
	for err, want := range map[error]string{
		nil: "ok",
		model.NewError(model.CodeUnauthorized, "x"): "unauthorized",
		model.NewError(model.CodeConflict, "x"):     "conflict",
		model.NewError(model.CodeInvalidInput, "x"): "invalid_input",
		model.NewError(model.CodeRateLimited, "x"):  "rate_limited",
		model.NewError(model.CodeNotFound, "x"):     "unavailable",
		errors.New("disk said no"):                  "unavailable",
	} {
		if got := v2Code(err); got != want {
			t.Errorf("v2Code(%v) = %q, want %q", err, got, want)
		}
	}
}

// v2ChildTestMain runs the test binary as a v2 plugin child.
func v2ChildTestMain() {
	if len(os.Args) != 4 || os.Args[1] != "v2-test-child" {
		return
	}
	if err := v2TestChild(os.Args[2], os.Args[3]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

type v2TestPeer struct {
	in     *bufio.Reader
	events map[string][]model.EventMessage // message id -> feed events, in order
}

// next acks feed offers until a host frame with key want arrives. With want
// empty it returns after one acked offer.
func (c v2TestPeer) next(want string) (json.RawMessage, error) {
	for {
		raw, err := c.in.ReadBytes('\n')
		if err != nil {
			return nil, err
		}
		var f map[string]json.RawMessage
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		if _, ok := f["id"]; !ok {
			if value, ok := f[want]; ok {
				return value, nil
			}
			return nil, fmt.Errorf("unexpected host frame %s", raw)
		}
		if err := c.ackOffer(f); err != nil || want == "" {
			return nil, err
		}
	}
}

// ackOffer records the message event of one feed offer and acks it.
func (c v2TestPeer) ackOffer(f map[string]json.RawMessage) error {
	var id string
	if err := json.Unmarshal(f["id"], &id); err != nil {
		return err
	}
	var e model.Event
	if raw, ok := f["event"]; ok && json.Unmarshal(raw, &e) == nil && e.Message != nil {
		c.events[e.Message.ID] = append(c.events[e.Message.ID], *e.Message)
	}
	return v2TestWrite(map[string]string{"ack": id})
}

func (c v2TestPeer) call(op string, args map[string]string, want string, out *v2Result) error {
	if err := v2TestWrite(map[string]map[string]string{op: args}); err != nil {
		return err
	}
	raw, err := c.next(want)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// seen stores the first feed event of message id that matches.
func (c v2TestPeer) seen(id string, match func(model.EventMessage) bool, out *model.EventMessage) bool {
	for _, m := range c.events[id] {
		if match(m) {
			*out = m
			return true
		}
	}
	return false
}

// drain acks offers until the host closes the stream.
func (c v2TestPeer) drain() {
	for {
		if _, err := c.next("none"); err != nil {
			return
		}
	}
}

func v2TestWrite(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(append(raw, '\n'))
	return err
}

// pollInbox calls inbox_next until a message arrives.
func (c v2TestPeer) pollInbox() (pluginfeed.PluginInboxMessage, error) {
	var m pluginfeed.PluginInboxMessage
	for i := 0; i <= 5; i++ {
		var r v2Result
		if err := c.call("inbox_next", map[string]string{"request_id": fmt.Sprintf("n%d", i)}, "inbox_result", &r); err != nil {
			return m, err
		}
		if r.Code == "ok" && r.Message != nil {
			raw, err := json.Marshal(r.Message)
			if err != nil {
				return m, err
			}
			return m, json.Unmarshal(raw, &m)
		}
		if r.Code != "ok" && r.Code != "rate_limited" {
			return m, fmt.Errorf("inbox_next: %s", r.Code)
		}
		time.Sleep(v2EmptyPoll + 100*time.Millisecond)
	}
	return m, errors.New("inbox_next: no message")
}

// waitEvents acks offers until the feed showed the consume and the send.
func (c v2TestPeer) waitEvents(out *v2TestOutcome) error {
	for !c.seen(out.Inbox.ID, func(m model.EventMessage) bool { return m.Acknowledged }, &out.Acked) ||
		!c.seen(fmt.Sprint(out.Send.Message), func(m model.EventMessage) bool { return m.Previous == "" }, &out.Created) {
		if _, err := c.next(""); err != nil {
			return fmt.Errorf("waiting for feed events: %w", err)
		}
	}
	return nil
}

func v2TestChild(resultPath, to string) error {
	if err := v2TestWrite(frame{Hello: &hello{Version: "v2"}}); err != nil {
		return err
	}
	c := v2TestPeer{bufio.NewReaderSize(os.Stdin, maxFrame+1), map[string][]model.EventMessage{}}
	if raw, err := c.next("hello"); err != nil || !bytes.Contains(raw, []byte(`"v2"`)) {
		return errors.New("missing v2 confirmation")
	}
	var out v2TestOutcome
	var err error
	if out.Inbox, err = c.pollInbox(); err != nil {
		return err
	}
	if err := c.call("inbox_consume", map[string]string{"request_id": "c", "message": out.Inbox.ID}, "inbox_consumed", &out.Consumed); err != nil {
		return err
	}
	// An answer under the reply rule: it names the inbox message it answers.
	send := map[string]string{"request_id": "s1", "client_message_id": "c1", "reply_to": out.Inbox.ID, "to": to, "body": "reply: " + out.Inbox.Body}
	if err := c.call("send", send, "send_result", &out.Send); err != nil {
		return err
	}
	send["request_id"] = "s2"
	if err := c.call("send", send, "send_result", &out.Resend); err != nil {
		return err
	}
	if err := c.waitEvents(&out); err != nil {
		return err
	}
	if err := v2TestWriteResult(resultPath, out); err != nil {
		return err
	}
	c.drain()
	return nil
}

func v2TestWriteResult(path string, out v2TestOutcome) error {
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", raw, 0600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}
