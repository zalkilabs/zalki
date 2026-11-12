package pluginfeed

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func (x inboxReadFixture) record(t *testing.T, id string) model.Message {
	t.Helper()
	var m model.Message
	err := bus.WithJournalRead(x.f.store, func(view bus.JournalView) error {
		var err error
		m, _, err = view.CurrentMessage(id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func (x inboxReadFixture) next(t *testing.T) *PluginInboxMessage {
	t.Helper()
	m, code := x.f.NextPluginInbox(&x.status)
	if code != "ok" {
		t.Fatalf("inbox_next code %q, want ok", code)
	}
	return m
}

func (x inboxReadFixture) consume(t *testing.T, id string) {
	t.Helper()
	if code := x.f.ConsumePluginInbox(&x.status, id); code != "ok" {
		t.Fatalf("inbox_consume code %q, want ok", code)
	}
}

// isFirst reports whether m is the fixture message with its core provenance.
func (x inboxReadFixture) isFirst(m *PluginInboxMessage) bool {
	return m != nil && m.ID == x.message.ID && m.From == x.from.ID && m.SenderGeneration == x.from.Generation &&
		m.To == x.message.To && m.Body == x.message.Body && m.CreatedAt == x.message.CreatedAt
}

func TestPluginInboxNextReplaysOldestUntilConsumed(t *testing.T) {
	x := newInboxReadFixture(t, "")
	second, err := x.service.Send(context.Background(), x.request())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if m := x.next(t); !x.isFirst(m) {
			t.Fatalf("inbox_next did not replay the oldest message with core provenance: %+v", m)
		}
	}
	x.consume(t, x.message.ID)
	if x.record(t, x.message.ID).AcknowledgedAt == "" {
		t.Fatal("consume did not acknowledge the record")
	}
	if entries := x.index(t).Entries; len(entries) != 1 || entries[0].ID != second.ID {
		t.Fatalf("consume did not drop exactly its index entry: %+v", entries)
	}
	x.consume(t, x.message.ID) // repeat is ok
	if m := x.next(t); m == nil || m.ID != second.ID {
		t.Fatalf("inbox_next after consume = %+v, want the second message", m)
	}
	x.consume(t, second.ID)
	if m := x.next(t); m != nil {
		t.Fatalf("empty inbox returned %+v", m)
	}
}

func TestPluginInboxConsumeRefusesUnknownIDs(t *testing.T) {
	x := newInboxReadFixture(t, "")
	if code := x.f.ConsumePluginInbox(&x.status, "not-a-uuid"); code != "invalid_input" {
		t.Fatalf("bad id code %q", code)
	}
	if code := x.f.ConsumePluginInbox(&x.status, model.NewUUID()); code != "not_found" {
		t.Fatalf("unknown id code %q", code)
	}
	if x.record(t, x.message.ID).AcknowledgedAt != "" || len(x.index(t).Entries) != 1 {
		t.Fatal("a refused consume changed the inbox")
	}
}

func TestPluginInboxNeedsInboxReadGrant(t *testing.T) {
	x := newInboxStorageFixture(t) // InboxFrom only, no InboxRead
	m, err := x.service.Send(context.Background(), x.request())
	if err != nil {
		t.Fatal(err)
	}
	if got, code := x.f.NextPluginInbox(&x.status); got != nil || code != "unauthorized" {
		t.Fatalf("next without inbox_read = %+v, %q", got, code)
	}
	if code := x.f.ConsumePluginInbox(&x.status, m.ID); code != "unauthorized" {
		t.Fatalf("consume without inbox_read = %q", code)
	}
	if len(x.index(t).Entries) != 1 {
		t.Fatal("a refused consume changed the index")
	}
}

func TestPluginInboxCrashAfterAckIsRepaired(t *testing.T) {
	x := newInboxReadFixture(t, "")
	// The record ACK is written, then the process dies before the index write.
	if err := x.f.write(func(view bus.JournalView) error {
		return view.AckPluginInboxMessage(x.f.store.Dir, x.status.ID, x.message.ID)
	}); err != nil {
		t.Fatal(err)
	}
	if len(x.index(t).Entries) != 1 {
		t.Fatal("fixture lost the index entry")
	}
	if m := x.next(t); m != nil {
		t.Fatalf("an acknowledged message was offered again: %+v", m)
	}
	if len(x.index(t).Entries) != 0 {
		t.Fatal("next did not drop the consumed entry")
	}
	x.consume(t, x.message.ID)
}

func TestPluginInboxStopsWhenDisabled(t *testing.T) {
	x := newInboxReadFixture(t, "")
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if m, code := x.f.NextPluginInbox(&x.status); m != nil || code != "revoked" {
		t.Fatalf("disabled next = %+v, %q", m, code)
	}
	if code := x.f.ConsumePluginInbox(&x.status, x.message.ID); code != "revoked" {
		t.Fatalf("disabled consume = %q", code)
	}
}

func TestPluginInboxRefusesBodiesTooLargeForOneFrame(t *testing.T) {
	x := newInboxReadFixture(t, "")
	r := x.request()
	r.Body = strings.Repeat("<", 16<<10) // 6 bytes each after JSON escaping
	if _, err := x.service.Send(context.Background(), r); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("oversized escaped body: %v", err)
	}
	x.assertAbsent(t, r.MessageID)
}

type pluginSendFixture struct {
	f                          PluginFeed
	store                      bus.Store
	service                    bus.Service
	status                     PluginFeedStatus
	granted, approval, retired model.Session
	absent, foreign, key       string
}

func newPluginSendFixture(t *testing.T) pluginSendFixture {
	t.Helper()
	f, _, _ := externalPinFixture(t)
	x := pluginSendFixture{f: f, store: f.store, absent: "cursor:" + model.NewUUID()}
	x.granted, x.key = register(t, f.store, "cursor")
	x.approval, _ = register(t, f.store, "cursor")
	x.retired, _ = register(t, f.store, "cursor")
	other, _ := register(t, f.store, "cursor")
	x.foreign = other.ID
	send := []string{x.granted.ID, x.approval.ID, x.retired.ID, x.absent}
	if err := f.GrantPluginCapabilities(PluginCapabilityGrants{Send: send}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	var err error
	if x.status, err = f.PluginStatus(); err != nil {
		t.Fatal(err)
	}
	x.service = x.boundService(x.status)
	return x
}

// boundService is the service a host builds for a child launched with binding.
func (x pluginSendFixture) boundService(binding PluginFeedStatus) bus.Service {
	return bus.Service{Store: x.store, PluginSend: BoundPluginSend(&binding)}
}

func (x pluginSendFixture) send(client, to, body string) (model.Message, error) {
	return x.service.SendFromPlugin(context.Background(), bus.PluginSendRequest{Installation: x.status.ID, Name: x.status.Name, ClientMessageID: client, To: to, Body: body})
}

func (x pluginSendFixture) messageCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(x.store.Dir, "messages"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(entries)
}

func TestPluginSendChecksTheGrantBeforeAnyLookup(t *testing.T) {
	x := newPluginSendFixture(t)
	for _, to := range []string{x.foreign, "cursor:" + model.NewUUID(), "plugin:" + x.status.ID} {
		if _, err := x.send("c1", to, "hello"); model.ErrorCodeOf(err) != model.CodeUnauthorized {
			t.Fatalf("send to %s not granted: %v", to, err)
		}
	}
	if _, err := x.send("c1", x.absent, "hello"); model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatalf("granted but unregistered recipient: %v", err)
	}
	if n := x.messageCount(t); n != 0 {
		t.Fatalf("refused sends wrote %d records", n)
	}
}

func TestPluginSendIsIdempotentAndShownAsPlugin(t *testing.T) {
	x := newPluginSendFixture(t)
	m, err := x.send("reply-1", x.granted.ID, "slot granted")
	if err != nil {
		t.Fatal(err)
	}
	x.checkIdentity(t, m, "reply-1")
	again, err := x.send("reply-1", x.granted.ID, "slot granted")
	if err != nil || again.ID != m.ID || again.Status != m.Status || x.messageCount(t) != 1 {
		t.Fatalf("retry was not deduplicated: %+v %v", again, err)
	}
	if _, err := x.send("reply-1", x.granted.ID, "slot refused"); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("changed body with the same client id: %v", err)
	}
	if _, err := x.send("reply-1", x.approval.ID, "slot granted"); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("changed recipient with the same client id: %v", err)
	}
}

// checkIdentity checks the core fields and the envelope of a plugin message.
func (x pluginSendFixture) checkIdentity(t *testing.T, m model.Message, client string) {
	t.Helper()
	if m.ID != model.DeriveUUID("zalki/plugin-send/v1", x.status.ID, client) || m.From != "plugin:"+x.status.ID || m.FromLabel != model.PluginLabel || m.FromName != x.status.Name || m.Status != model.StatusPendingActivity {
		t.Fatalf("plugin message has wrong identity or status: %+v", m)
	}
	text := model.Envelope(m)
	if !strings.Contains(text, "From: Plugin · \""+x.status.Name+"\"") || !strings.Contains(text, "Plugin message; not user approval.") || strings.Contains(text, "Agent message") {
		t.Fatalf("envelope does not show a plugin sender:\n%s", text)
	}
}

func TestPluginSendFollowsApprovalAndRetirement(t *testing.T) {
	x := newPluginSendFixture(t)
	if err := x.store.Update(x.approval.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	if m, err := x.send("held", x.approval.ID, "wait"); err != nil || m.Status != model.StatusApprovalBlocked || !m.Held {
		t.Fatalf("send to a session in approval: %+v %v", m, err)
	}
	if err := x.store.Update(x.retired.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if _, err := x.send("gone", x.retired.ID, "late"); model.ErrorCodeOf(err) != model.CodeUnavailable {
		t.Fatalf("send to a retired session: %v", err)
	}
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if _, err := x.send("off", x.granted.ID, "hello"); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatalf("send from a disabled plugin: %v", err)
	}
}

func TestPluginSendObeysTheConfiguredBodyLimit(t *testing.T) {
	x := newPluginSendFixture(t)
	x.service.Store.MaxBodyBytes = 4
	if _, err := x.send("big", x.granted.ID, "12345"); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("body over the configured limit: %v", err)
	}
	if n := x.messageCount(t); n != 0 {
		t.Fatalf("a refused body wrote %d records", n)
	}
	m, err := x.send("fits", x.granted.ID, "1234")
	if err != nil {
		t.Fatal(err)
	}
	x.service.Store.MaxBodyBytes = 2 // like send_message, a retry returns the stored record
	if again, err := x.send("fits", x.granted.ID, "1234"); err != nil || again.ID != m.ID {
		t.Fatalf("retry after the limit was lowered: %+v %v", again, err)
	}
}

func TestPluginSendIsBoundToTheLaunch(t *testing.T) {
	x := newPluginSendFixture(t) // x.service is bound to the launch-time status
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := x.f.GrantPluginCapabilities(PluginCapabilityGrants{Send: []string{x.granted.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if _, err := x.send("stale", x.granted.ID, "hello"); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatalf("send from a child of the old grant revision: %v", err)
	}
	other := x.status
	other.ID = model.NewUUID()
	request := bus.PluginSendRequest{Installation: x.status.ID, Name: x.status.Name, ClientMessageID: "other", To: x.granted.ID, Body: "hello"}
	service := x.boundService(other)
	if _, err := service.SendFromPlugin(context.Background(), request); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatalf("send for another installation's binding: %v", err)
	}
	if n := x.messageCount(t); n != 0 {
		t.Fatalf("refused sends wrote %d records", n)
	}
	current, err := x.f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	service = x.boundService(current)
	if _, err := service.SendFromPlugin(context.Background(), request); err != nil {
		t.Fatalf("send from a child of the current launch: %v", err)
	}
}

// addPlugin installs and enables a second plugin in the same Store.
func addPlugin(t *testing.T, store bus.Store, name, executable string, feed []string, grants PluginCapabilityGrants) (PluginFeed, PluginFeedStatus) {
	t.Helper()
	p := NewPluginFeed(store, name)
	if err := p.AddExternalPlugin(executable, nil); err != nil {
		t.Fatal(err)
	}
	if err := p.ConfigurePluginFeed(feed); err != nil {
		t.Fatal(err)
	}
	if err := p.GrantPluginCapabilities(grants); err != nil {
		t.Fatal(err)
	}
	if err := p.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := p.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	return p, status
}

// readAt is one bound read at a chosen time (reads are limited to one a second).
func readAt(t *testing.T, p PluginFeed, before *PluginFeedStatus, session, id string, now time.Time) ([]byte, string) {
	t.Helper()
	var raw []byte
	var code string
	if err := p.write(func(view bus.JournalView) error {
		raw, code, _ = p.readPluginMessageAt(view, before, session, id, model.NewUUID(), now)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return raw, code
}

// pluginMailFixture sends a private plugin message to a native session, then
// installs a different plugin with a valid Read grant for that session.
func pluginMailFixture(t *testing.T) (pluginSendFixture, model.Message, PluginFeed, PluginFeedStatus) {
	t.Helper()
	x := newPluginSendFixture(t)
	m, err := x.send("private", x.granted.ID, "PRIVATE_SEND_BODY")
	if err != nil {
		t.Fatal(err)
	}
	reader, status := addPlugin(t, x.store, "reader", x.status.Launch.Executable, []string{x.granted.ID}, PluginCapabilityGrants{Read: []string{x.granted.ID}})
	if status.ID == x.status.ID {
		t.Fatal("the reader must be a different installation")
	}
	return x, m, reader, status
}

// The owner proxy (zalki plugins read) refuses plugin mail the same way.
func TestPluginOwnerReadRefusesAnotherPluginsMail(t *testing.T) {
	x, m, reader, _ := pluginMailFixture(t)
	raw, err := reader.ReadPluginMessage(x.granted.ID, m.ID)
	if model.ErrorCodeOf(err) != model.CodeUnavailable || raw != nil || strings.Contains(fmt.Sprint(err), "PRIVATE") {
		t.Fatalf("owner read gave another plugin's mail: %s %v", raw, err)
	}
}

// A different plugin with a valid Read grant for the recipient session must
// not read a plugin-sent message; native mail stays readable with that grant,
// and the native recipient still gets the plugin message.
func TestPluginReadRefusesAnotherPluginsMail(t *testing.T) {
	x, m, reader, status := pluginMailFixture(t)
	now := time.Now().UTC()
	if raw, code := readAt(t, reader, &status, x.granted.ID, m.ID, now); code != "unavailable" || strings.Contains(string(raw), "PRIVATE") {
		t.Fatalf("another plugin read plugin mail: %s %s", code, raw)
	}
	sender, key := register(t, x.store, "cursor")
	service := bus.Service{Store: x.store}
	native, err := service.Send(context.Background(), bus.SendRequest{Actor: sender.ID, Capability: key, To: x.granted.ID, MessageID: model.NewUUID(), Body: "native body"})
	if err != nil {
		t.Fatal(err)
	}
	if raw, code := readAt(t, reader, &status, x.granted.ID, native.ID, now.Add(2*time.Second)); code != "ok" || !strings.Contains(string(raw), "native body") {
		t.Fatalf("the Read grant no longer reads native mail: %s %s", code, raw)
	}
	inbox, err := x.store.Messages(x.granted.ID, x.key)
	if err != nil || !slices.ContainsFunc(inbox, func(v model.Message) bool { return v.ID == m.ID && v.Body == "PRIVATE_SEND_BODY" }) {
		t.Fatalf("the native recipient lost the plugin message: %v", err)
	}
}

// inbox_next and inbox_consume see only the plugin's own inbox.
func TestPluginInboxNeverReturnsAnotherPluginsMail(t *testing.T) {
	x := newInboxReadFixture(t, "")
	other, status := addPlugin(t, x.f.store, "other", x.status.Launch.Executable, []string{x.from.ID}, PluginCapabilityGrants{InboxRead: true, InboxFrom: []string{x.from.ID}})
	r := x.request()
	r.To, r.Body = "plugin:"+status.ID, "PRIVATE_B"
	b, err := x.service.Send(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if code := x.f.ConsumePluginInbox(&x.status, b.ID); code != "not_found" {
		t.Fatalf("consume of another plugin's message = %q", code)
	}
	x.consume(t, x.message.ID)
	if m := x.next(t); m != nil {
		t.Fatalf("inbox_next returned another plugin's message: %+v", m)
	}
	if m, code := other.NextPluginInbox(&status); code != "ok" || m == nil || m.ID != b.ID {
		t.Fatalf("the other plugin lost its own message: %+v %q", m, code)
	}
}

func TestPluginMessageTakesNoReply(t *testing.T) {
	x := newPluginSendFixture(t)
	m, err := x.send("ask", x.granted.ID, "question")
	if err != nil {
		t.Fatal(err)
	}
	_, err = x.service.Reply(context.Background(), bus.ReplyRequest{Actor: x.granted.ID, Capability: x.key, MessageID: m.ID, Body: "answer"})
	if model.ErrorCodeOf(err) != model.CodeInvalidInput || !strings.Contains(err.Error(), "send_message to plugin:"+x.status.ID) {
		t.Fatalf("reply to a plugin message: %v", err)
	}
}
