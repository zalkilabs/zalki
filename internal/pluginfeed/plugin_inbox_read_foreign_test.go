package pluginfeed

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInboxReadForeignAndAbsentIDsHaveSameFixedDenial(t *testing.T) {
	x := newInboxReadFixture(t, "")
	other, status := secondInboxReadOwner(t, x)
	native := foreignNativeInboxMessage(t, x)
	before := journalSnapshot(t, x.f.store)
	first := ""
	ids := []string{model.NewUUID(), x.message.ID, native.ID, model.NewUUID()}
	for i, id := range ids {
		var raw []byte
		var readErr error
		if err := other.write(func(view bus.JournalView) error {
			raw, readErr = other.readPluginInboxAt(view, &status, id, x.now.Add(time.Duration(2*i)*time.Second))
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if len(raw) != 0 || model.ErrorCodeOf(readErr) != model.CodeUnauthorized || !strings.Contains(readErr.Error(), "plugin read denied") {
			t.Fatal("foreign/absent ID exposed body or varied denial", len(raw), readErr)
		}
		if i == 0 {
			first = readErr.Error()
		} else if readErr.Error() != first {
			t.Fatal("absence/native/cross-plugin denial differed")
		}
	}
	after := journalSnapshot(t, x.f.store)
	delete(after, "plugin-inbox-read-audit.json")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("foreign read changed canonical/native/plugin indexes or state")
	}
	checkForeignInboxReadAudit(t, x.audit(t), status, ids)
}
func checkForeignInboxReadAudit(t *testing.T, log pluginReadAudit, status PluginFeedStatus, ids []string) {
	t.Helper()
	if len(log.Rows) != len(ids) {
		t.Fatal("fixed denials were not individually audited")
	}
	for i, row := range log.Rows {
		if row.Plugin != status.ID || row.Session != "plugin:"+status.ID || row.Message != ids[i] || row.Outcome != "denied" || row.Bytes != 0 {
			t.Fatal("foreign refusal audit has wrong recipient/outcome")
		}
	}
}

func secondInboxReadOwner(t *testing.T, x inboxReadFixture) (PluginFeed, PluginFeedStatus) {
	t.Helper()
	f := NewPluginFeed(x.f.store, "second")
	if err := f.AddExternalPlugin(x.status.Launch.Executable, x.status.Launch.Args); err != nil {
		t.Fatal(err)
	}
	if err := f.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := f.GrantPluginAccess(PluginAccess{Status: true, Receive: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if err = status.Launch.Verify(); err != nil {
		t.Fatal(err)
	}
	return f, status
}
func foreignNativeInboxMessage(t *testing.T, x inboxReadFixture) model.Message {
	t.Helper()
	target, _ := register(t, x.f.store, "cursor")
	service := bus.Service{Store: x.f.store, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		return model.NewReceipt(model.StatusSubmitted)
	}}
	m, err := service.Send(context.Background(), bus.SendRequest{Actor: x.from.ID, Capability: x.key, To: target.ID, MessageID: model.NewUUID(), Body: "PRIVATE-NATIVE-FOREIGN-INBOX"})
	if err != nil {
		t.Fatal(err)
	}
	return m
}
