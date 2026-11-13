package pluginfeed

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// TestNativeSendCannotTakeAServerDerivedID checks that a native sender cannot
// choose a version 8 id, the version of every id the server derives, so it
// cannot block a plugin's send by creating its id first. Ids that agents write
// themselves (versions 1, 4 and 7) still work.
func TestNativeSendCannotTakeAServerDerivedID(t *testing.T) {
	x := newPluginSendFixture(t)
	sender, key := register(t, x.store, "cursor")
	native := bus.Service{Store: x.store}
	send := func(id string) error {
		_, err := native.Send(context.Background(), bus.SendRequest{Actor: sender.ID, Capability: key, To: x.granted.ID, MessageID: id, Body: "hello"})
		return err
	}
	taken := model.DeriveUUID("zalki/plugin-send/v1", x.status.ID, "c-take")
	if err := send(taken); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatal("a native sender took a server-derived id", err)
	}
	for _, id := range []string{"123e4567-e89b-12d3-a456-426614174000", model.NewUUID(), "01890a5d-ac96-774b-bcce-b302099a8057", ""} {
		if err := send(id); err != nil {
			t.Fatal("a normal message id was refused", id, err)
		}
	}
	m, err := x.send("c-take", x.granted.ID, "slot granted")
	if err != nil || m.ID != taken {
		t.Fatal("the plugin could not send with its own id", m.ID, err)
	}
}

// TestRefusedReadsAreCappedPerPlugin floods refused reads after one released
// read: within a second they are limited without a row, and over time the
// plugin keeps at most 64 refused rows. With the audit below its global
// limit, the released row and another plugin's row stay.
func TestRefusedReadsAreCappedPerPlugin(t *testing.T) {
	x := newPluginReadFixture(t, "cursor")
	if _, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now); err != nil {
		t.Fatal(err)
	}
	other := pluginReadAuditRow{At: x.now, Plugin: model.NewUUID(), Request: model.NewUUID(), Session: x.target.ID, Message: x.message.ID, Revision: strings.Repeat("e", 64), Outcome: "denied"}
	if err := x.f.savePluginReadAudit(append(x.audit(t, x.now), other), x.now); err != nil {
		t.Fatal(err)
	}
	outside := "cursor:" + model.NewUUID()
	if _, err := x.read(t, &x.before, outside, x.message.ID, x.now.Add(500*time.Millisecond)); model.ErrorCodeOf(err) != model.CodeRateLimited {
		t.Fatal("a refused read inside the window was not limited", err)
	}
	if rows := x.audit(t, x.now); len(rows) != 2 {
		t.Fatal("a limited read wrote a row", len(rows))
	}
	last := x.now
	for i := 1; i <= 1500; i++ {
		last = x.now.Add(time.Duration(i) * time.Second)
		if _, err := x.read(t, &x.before, outside, x.message.ID, last); err == nil {
			t.Fatal("a read outside the grant was released")
		}
	}
	rows := x.audit(t, last)
	refused, released, others := 0, 0, 0
	for _, row := range rows {
		switch {
		case row.Plugin == other.Plugin:
			others++
		case row.Outcome == "ok":
			released++
		default:
			refused++
		}
	}
	if refused != maxRefusedReadRows || released != 1 || others != 1 {
		t.Fatalf("audit after the flood: %d refused, %d released, %d of another plugin", refused, released, others)
	}
}

// TestRefusedReadCapShrinksAnOlderLargerAudit starts from an audit that holds
// more refused rows of one plugin than the cap (written before the cap). One
// more refused read brings it back to the cap; the released row stays.
func TestRefusedReadCapShrinksAnOlderLargerAudit(t *testing.T) {
	x := newPluginReadFixture(t, "cursor")
	if _, err := x.read(t, &x.before, x.target.ID, x.message.ID, x.now); err != nil {
		t.Fatal(err)
	}
	rows := x.audit(t, x.now)
	other := pluginReadAuditRow{At: x.now, Plugin: model.NewUUID(), Request: model.NewUUID(), Session: x.target.ID, Message: x.message.ID, Revision: strings.Repeat("e", 64), Outcome: "denied"}
	rows = append(rows, other)
	for i := 0; i < 96; i++ {
		rows = append(rows, pluginReadAuditRow{At: x.now.Add(time.Duration(i+1) * time.Millisecond), Plugin: x.before.ID, Request: model.NewUUID(), Session: x.target.ID, Message: x.message.ID, Revision: x.before.Revision, Outcome: "denied"})
	}
	if err := x.f.savePluginReadAudit(rows, x.now); err != nil {
		t.Fatal(err)
	}
	later := x.now.Add(2 * time.Second)
	if _, err := x.read(t, &x.before, "cursor:"+model.NewUUID(), x.message.ID, later); err == nil {
		t.Fatal("a read outside the grant was released")
	}
	refused, released, others := 0, 0, 0
	for _, row := range x.audit(t, later) {
		switch {
		case row.Plugin == other.Plugin:
			others++
		case row.Outcome == "ok":
			released++
		default:
			refused++
		}
	}
	if refused != maxRefusedReadRows || released != 1 || others != 1 {
		t.Fatalf("audit after one more refused read: %d refused, %d released, %d of another plugin", refused, released, others)
	}
}
