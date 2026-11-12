package bus_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// The privacy boundary is exercised through public operations and persisted
// records, without calling admission/index implementation helpers.
type inboxPublicFixture struct {
	service              *bus.Service
	feed                 pluginfeed.PluginFeed
	author, other, third agent
	to, installation     string
}

func newInboxPublicFixture(t *testing.T) inboxPublicFixture {
	t.Helper()
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	author := register(t, store, "cursor")
	other := register(t, store, "codex")
	third := register(t, store, "cursor")
	executable := filepath.Join(t.TempDir(), "private-plugin")
	if err = os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "inbox-fixture")
	if err = feed.AddExternalPlugin(executable, nil); err != nil {
		t.Fatal(err)
	}
	if err = feed.ConfigurePluginFeed([]string{author.ID}); err != nil {
		t.Fatal(err)
	}
	if err = feed.GrantPluginCapabilities(pluginfeed.PluginCapabilityGrants{Receive: []string{author.ID}}); err != nil {
		t.Fatal(err)
	}
	if err = feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	return inboxPublicFixture{service: &bus.Service{Store: store, PluginInbox: pluginfeed.AdmitPluginInbox}, feed: feed, author: author, other: other, third: third, to: "plugin:" + status.ID, installation: status.ID}
}

func (x inboxPublicFixture) send(t *testing.T, body string) model.Message {
	t.Helper()
	x.service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		t.Error("plugin message invoked a native transport")
		return model.NewReceipt(model.StatusSubmitted)
	}
	result, err := x.service.Send(context.Background(), bus.SendRequest{Actor: x.author.ID, Capability: x.author.Key, To: x.to, MessageID: newUUID(t), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != model.StatusSubmitted || result.AcknowledgedAt != "" || result.Held || result.To != x.to || result.From != x.author.ID || result.TargetGeneration != x.installation || result.Body != body || result.Receipt == nil || result.Receipt.Status != model.StatusSubmitted || result.Receipt.Output != "" {
		t.Fatal("plugin receipt promised delivery or lost storage identity")
	}
	return result
}

func TestPluginInboxAuthorKeepsOwnHistory(t *testing.T) {
	x := newInboxPublicFixture(t)
	body := "PRIVATE-PLUGIN-INBOX-BODY\nsecond line"
	message := x.send(t, body)
	store := x.service.Store
	for _, mode := range []string{"exact", "history", "sent", "preview", "summary", "inbox"} {
		t.Run(mode, func(t *testing.T) {
			checkInboxAuthorMode(t, x, message, body, mode)
		})
	}
	if _, err := store.QueryMessages(bus.MessageQuery{Actor: x.author.ID, Capability: x.author.Key, Sent: true, To: x.to}); err == nil {
		t.Fatal("cut1 unexpectedly added plugin sent-history To-filter support")
	}
}

func TestPluginInboxNonAuthorsCannotReadOrConsume(t *testing.T) {
	x := newInboxPublicFixture(t)
	message := x.send(t, "PRIVATE-PLUGIN-INBOX-BODY")
	store := x.service.Store
	canonical := filepath.Join(store.Dir, "messages", message.ID+".json")
	before, err := os.ReadFile(canonical)
	if err != nil {
		t.Fatal(err)
	}
	for _, who := range []agent{x.other, x.third} {
		checkInboxNonAuthorQueries(t, x, who, message.ID)
	}
	for _, who := range []agent{x.author, x.other, x.third} {
		checkInboxNativeMutators(t, x, who, message.ID)
	}
	after, err := os.ReadFile(canonical)
	if err != nil || string(before) != string(after) {
		t.Fatal("native operation changed plugin canonical record")
	}
	agents, err := store.Discover()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range agents {
		if strings.HasPrefix(row.ID, "plugin:") {
			t.Fatal("plugin appeared in list_agents")
		}
	}
	if _, err = os.Stat(filepath.Join(store.Dir, "sessions", "plugin_"+x.installation+".json")); !os.IsNotExist(err) {
		t.Fatal("plugin was installed as a native session")
	}
}

func TestPluginInboxCannotBorrowNativeCapability(t *testing.T) {
	x := newInboxPublicFixture(t)
	message := x.send(t, "PRIVATE-PLUGIN-INBOX-BODY")
	store := x.service.Store
	before := snapshot(t, store.Dir)
	forged := bus.MessageQuery{Actor: x.to, Capability: x.author.Key, MessageID: message.ID, IncludeBody: true}
	if rows, err := store.QueryMessages(forged); err == nil || len(rows) != 0 {
		t.Fatal("plugin actor borrowed native read authority")
	}
	if result, err := store.Ack(x.to, x.author.Key, message.ID); err == nil || !reflect.DeepEqual(result, model.Message{}) {
		t.Fatal("plugin actor borrowed native ACK authority")
	}
	if result, err := x.service.Send(context.Background(), bus.SendRequest{Actor: x.to, Capability: x.author.Key, To: x.other.ID, MessageID: newUUID(t), Body: "forged plugin sender"}); err == nil || !reflect.DeepEqual(result, model.Message{}) {
		t.Fatal("plugin actor borrowed native send authority")
	}
	if _, _, err := store.Register("plugin", x.installation, "/workspace", nil); err == nil {
		t.Fatal("native registry admitted plugin actor")
	}
	if after := snapshot(t, store.Dir); !reflect.DeepEqual(before, after) {
		t.Fatal("forged native credentials changed state")
	}
}

func TestPluginInboxNativeReadGrantDoesNotBecomeSentAccess(t *testing.T) {
	x := newInboxPublicFixture(t)
	message := x.send(t, "PRIVATE-PLUGIN-INBOX-BODY")
	if err := x.feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := x.feed.GrantPluginCapabilities(pluginfeed.PluginCapabilityGrants{Read: []string{x.author.ID}, Receive: []string{x.author.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := x.feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if raw, err := x.feed.ReadPluginMessage(x.author.ID, message.ID); err == nil || len(raw) != 0 {
		t.Fatal("native recipient read grant exposed authored plugin message")
	}
}

func checkInboxAuthorMode(t *testing.T, x inboxPublicFixture, message model.Message, body, mode string) {
	t.Helper()
	store := x.service.Store

	q := inboxAuthorQuery(x, message.ID, mode)
	rows, err := store.QueryMessages(q)
	if err != nil {
		t.Fatal(err)
	}
	if mode == "inbox" {
		if len(rows) != 0 {
			t.Fatal("plugin message appeared in native author inbox")
		}
		return
	}
	if len(rows) != 1 || rows[0].ID != message.ID {
		t.Fatal("author lost own plugin history")
	}
	if mode == "preview" {
		if !strings.Contains(rows[0].BodyPreview, "PRIVATE-PLUGIN-INBOX-BODY") || strings.ContainsRune(rows[0].BodyPreview, '\n') || len(rows[0].BodyPreview) > 80 {
			t.Fatal("author preview missing or unbounded")
		}
	}
	if mode == "preview" || mode == "summary" {
		if rows[0].Body != "" || !rows[0].BodyOmitted {
			t.Fatal("history summary repeated body")
		}
	} else if rows[0].Body != body {
		t.Fatal("author body was hidden")
	}

}

func checkInboxNonAuthorQueries(t *testing.T, x inboxPublicFixture, who agent, id string) {
	t.Helper()
	store := x.service.Store

	for _, mode := range []string{"exact", "history", "sent", "preview", "inbox"} {
		q := bus.MessageQuery{Actor: who.ID, Capability: who.Key, Limit: 100, IncludeBody: true}
		switch mode {
		case "exact":
			q.MessageID = id
		case "history":
			q.History = true
		case "sent":
			q.Sent = true
		case "preview":
			q.Sent = true
			q.Preview = true
		}
		rows, err := store.QueryMessages(q)
		if mode == "exact" && err == nil {
			t.Fatal("non-author exact read accepted")
		}
		if len(rows) != 0 {
			t.Fatal("non-author saw a plugin message")
		}
	}

}

func checkInboxNativeMutators(t *testing.T, x inboxPublicFixture, who agent, id string) {
	t.Helper()
	store := x.service.Store

	result, err := store.Ack(who.ID, who.Key, id)
	if model.ErrorCodeOf(err) != model.CodeNotFound || !reflect.DeepEqual(result, model.Message{}) {
		t.Fatal("failed plugin ACK returned message data")
	}
	result, err = x.service.Reply(context.Background(), bus.ReplyRequest{Actor: who.ID, Capability: who.Key, MessageID: id, Body: "not a plugin reply"})
	if err == nil || !reflect.DeepEqual(result, model.Message{}) {
		t.Fatal("native plugin reply consumed or returned message data")
	}
	if _, err = x.service.Watch(who.ID, who.Key, x.to); err == nil {
		t.Fatal("plugin became a native watch target")
	}

}

func inboxAuthorQuery(x inboxPublicFixture, id, mode string) bus.MessageQuery {
	q := bus.MessageQuery{Actor: x.author.ID, Capability: x.author.Key, Limit: 100, IncludeBody: true}
	switch mode {
	case "exact":
		q.MessageID = id
	case "history":
		q.History = true
	case "sent":
		q.Sent = true
	case "preview":
		q.Sent = true
		q.Preview = true
		q.IncludeBody = false
	case "summary":
		q.Sent = true
		q.IncludeBody = false
	}
	return q
}
