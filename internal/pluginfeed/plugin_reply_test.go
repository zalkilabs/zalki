package pluginfeed

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// replyFixture is a plugin with a receive grant for one session, a message
// from that session in its inbox, and a send path bound to its launch.
type replyFixture struct {
	inboxReadFixture
	sender bus.Service
}

func newReplyFixture(t *testing.T) replyFixture {
	t.Helper()
	x := newInboxReadFixture(t, "")
	sender := bus.Service{Store: x.f.store, PluginSend: BoundPluginSend(&x.status), Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		return model.NewReceipt(model.StatusSubmitted)
	}}
	return replyFixture{x, sender}
}

func (x replyFixture) reply(clientID, parent, to string) (model.Message, error) {
	return x.sender.SendFromPlugin(context.Background(), bus.PluginSendRequest{Installation: x.status.ID, Name: "external", ClientMessageID: clientID, ReplyTo: parent, To: to, Body: "answer " + clientID})
}

func atReplyTime(t *testing.T, now time.Time) {
	t.Helper()
	before := replyClock
	replyClock = func() time.Time { return now }
	t.Cleanup(func() { replyClock = before })
}

func TestPluginReplyIsStoredWithItsParent(t *testing.T) {
	x := newReplyFixture(t)
	m, err := x.reply("c1", x.message.ID, x.from.ID)
	if err != nil || m.ReplyTo != x.message.ID || m.To != x.from.ID || m.From != "plugin:"+x.status.ID {
		t.Fatalf("reply = %+v, %v", m, err)
	}
	// A retry returns the same message and does not count again.
	again, err := x.reply("c1", x.message.ID, x.from.ID)
	if err != nil || again.ID != m.ID {
		t.Fatal("retry", again, err)
	}
	ledger, err := x.f.loadReplyLedger(x.status.ID, time.Now())
	if err != nil || len(ledger.Parents[x.message.ID].Replies) != 1 {
		t.Fatal("ledger after a retry", ledger, err)
	}
}

func TestPluginReplyRuleRefusals(t *testing.T) {
	x := newReplyFixture(t)
	other, otherKey := register(t, x.f.store, "cursor")
	cases := []struct {
		name, parent, to, code string
	}{
		{"unknown parent", model.NewUUID(), x.from.ID, "not_a_reply"},
		{"to is not the parent sender", x.message.ID, other.ID, "not_a_reply"},
		{"no reply_to and no send grant", "", x.from.ID, ""},
	}
	for i, c := range cases {
		_, err := x.reply("r"+strconv.Itoa(i), c.parent, c.to)
		if err == nil || PluginSendCode(err) != c.code || (c.code == "" && model.ErrorCodeOf(err) != model.CodeUnauthorized) {
			t.Errorf("%s: %v (code %q)", c.name, err, PluginSendCode(err))
		}
	}
	// A message the other session sent to a different address is no parent.
	native, err := x.service.Send(context.Background(), bus.SendRequest{Actor: other.ID, Capability: otherKey, To: x.from.ID, MessageID: model.NewUUID(), Body: "not to the plugin"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.reply("native-parent", native.ID, other.ID); PluginSendCode(err) != "not_a_reply" {
		t.Fatal("a message to another session counted as a parent", err)
	}
}

func TestPluginReplyWindowAndLimit(t *testing.T) {
	x := newReplyFixture(t)
	created, err := time.Parse(time.RFC3339Nano, x.message.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	for i := range maxReplies {
		if _, err := x.reply("ok"+strconv.Itoa(i), x.message.ID, x.from.ID); err != nil {
			t.Fatal(i, err)
		}
	}
	if _, err := x.reply("ninth", x.message.ID, x.from.ID); PluginSendCode(err) != "reply_limit" {
		t.Fatal("ninth reply", err)
	}
	// After the window a counted reply still returns (a retry), a new one is late.
	atReplyTime(t, created.Add(replyWindow+time.Second))
	if _, err := x.reply("ok0", x.message.ID, x.from.ID); err != nil {
		t.Fatal("retry after the window", err)
	}
	if _, err := x.reply("late", x.message.ID, x.from.ID); PluginSendCode(err) != "reply_expired" {
		t.Fatal("late reply", err)
	}
	if _, err := x.reply("late", x.message.ID, x.from.ID); PluginSendCode(err) != "reply_expired" {
		t.Fatal("retry of a late reply", err)
	}
	atReplyTime(t, created.Add(replyWindow-time.Second))
	y := newReplyFixture(t)
	if _, err := y.reply("in-time", y.message.ID, y.from.ID); err != nil {
		t.Fatal("reply inside the window", err)
	}
}

// A sender that is no longer in the receive list gets no reply.
func TestPluginReplyNeedsTheSenderInScopeNow(t *testing.T) {
	x := newReplyFixture(t)
	other, _ := register(t, x.f.store, "cursor")
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := x.f.GrantPluginCapabilities(PluginCapabilityGrants{Receive: []string{other.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := x.f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	x.sender.PluginSend = BoundPluginSend(&status)
	x.status = status
	if _, err := x.reply("gone", x.message.ID, x.from.ID); PluginSendCode(err) != "out_of_scope" {
		t.Fatal("reply to a sender out of scope", err)
	}
}

// A send without reply_to needs the extended send grant for that recipient.
func TestPluginUnsolicitedSendNeedsSendGrant(t *testing.T) {
	x := newReplyFixture(t)
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := x.f.GrantPluginCapabilities(PluginCapabilityGrants{Receive: []string{x.from.ID}, Send: []string{x.from.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := x.f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	x.sender.PluginSend = BoundPluginSend(&status)
	x.status = status
	m, err := x.reply("push", "", x.from.ID)
	if err != nil || m.ReplyTo != "" {
		t.Fatal("unsolicited send with a send grant", m, err)
	}
}

// The ledger is host-owned: it is next to the record, not in the plugin's data.
func TestPluginReplyLedgerLocation(t *testing.T) {
	x := newReplyFixture(t)
	if _, err := x.reply("c1", x.message.ID, x.from.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(x.f.replyLedgerPath()); err != nil {
		t.Fatal("no ledger next to the record", err)
	}
	if _, err := os.Stat(x.f.pluginDataPath() + "/replies.json"); !os.IsNotExist(err) {
		t.Fatal("ledger in the plugin's data folder", err)
	}
	if err := os.WriteFile(x.f.replyLedgerPath(), []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := x.reply("c2", x.message.ID, x.from.ID); err == nil {
		t.Fatal("a broken ledger allowed a reply")
	}
}

// The count is written before the message: when the count cannot be written,
// no reply is stored (the limit fails closed).
func TestPluginReplyCountFailsClosed(t *testing.T) {
	x := newReplyFixture(t)
	dir := x.f.pluginDir()
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	if _, err := x.reply("blocked", x.message.ID, x.from.ID); err == nil {
		t.Fatal("a reply was sent although its count could not be written")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id := model.DeriveUUID("zalki/plugin-send/v1", x.status.ID, "blocked")
	if _, err := os.Stat(x.f.store.Dir + "/messages/" + id + ".json"); !os.IsNotExist(err) {
		t.Fatal("the reply was stored without its count", err)
	}
	if _, err := x.reply("blocked", x.message.ID, x.from.ID); err != nil {
		t.Fatal("retry after the disk error", err)
	}
	ledger, err := x.f.loadReplyLedger(x.status.ID, time.Now())
	if err != nil || len(ledger.Parents[x.message.ID].Replies) != 1 {
		t.Fatal("the retry was counted twice or not at all", ledger, err)
	}
}

func TestOldRecordFixQuotesThePath(t *testing.T) {
	for in, want := range map[string]string{
		"/fixture/dev/zalki-plugins/arbiter": "/fixture/dev/zalki-plugins/arbiter",
		"/fixture/my plugins/arbiter":        "'/fixture/my plugins/arbiter'",
		"/fixture/bob's/arbiter":             `'/fixture/bob'"'"'s/arbiter'`,
		"/tmp/$(touch x)":                    "'/tmp/$(touch x)'",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}

// failedCountedReply makes a reply that is counted but never stored: the body
// passes the model check, then the store's smaller body limit refuses it.
func failedCountedReply(t *testing.T, x replyFixture, clientID string) {
	t.Helper()
	limit := x.sender.Store.MaxBodyBytes
	x.sender.Store.MaxBodyBytes = 1
	if _, err := x.reply(clientID, x.message.ID, x.from.ID); err == nil {
		t.Fatal("the oversized reply was stored")
	}
	x.sender.Store.MaxBodyBytes = limit
	ledger, err := x.f.loadReplyLedger(x.status.ID, time.Now())
	id := model.DeriveUUID("zalki/plugin-send/v1", x.status.ID, clientID)
	if err != nil || len(ledger.Parents[x.message.ID].Replies) != 1 || ledger.Parents[x.message.ID].Replies[0] != id {
		t.Fatal("the failed reply was not counted", ledger, err)
	}
}

// A counted but never stored reply is no stored retry: when its sender left
// the receive list, the retry is refused.
func TestPluginReplyCountedButNotStoredRechecksScope(t *testing.T) {
	x := newReplyFixture(t)
	failedCountedReply(t, x, "c")
	other, _ := register(t, x.f.store, "cursor")
	if err := x.f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := x.f.GrantPluginCapabilities(PluginCapabilityGrants{Receive: []string{other.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := x.f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	x.sender.PluginSend, x.status = BoundPluginSend(&status), status
	if _, err := x.reply("c", x.message.ID, x.from.ID); PluginSendCode(err) != "out_of_scope" {
		t.Fatal("a counted, never stored reply skipped the scope check", err)
	}
}

// ... and after 24 h (but before the ledger drops it at 25 h) it is late.
func TestPluginReplyCountedButNotStoredRechecksAge(t *testing.T) {
	x := newReplyFixture(t)
	failedCountedReply(t, x, "c")
	created, err := time.Parse(time.RFC3339Nano, x.message.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	atReplyTime(t, created.Add(replyWindow+time.Minute))
	if _, err := x.reply("c", x.message.ID, x.from.ID); PluginSendCode(err) != "reply_expired" {
		t.Fatal("a counted, never stored reply skipped the age check", err)
	}
	// Inside the window the same retry is stored, and the count stays one.
	atReplyTime(t, created.Add(time.Minute))
	if _, err := x.reply("c", x.message.ID, x.from.ID); err != nil {
		t.Fatal("retry inside the window", err)
	}
	ledger, err := x.f.loadReplyLedger(x.status.ID, created.Add(time.Minute))
	if err != nil || len(ledger.Parents[x.message.ID].Replies) != 1 {
		t.Fatal("the retry was counted twice", ledger, err)
	}
}
