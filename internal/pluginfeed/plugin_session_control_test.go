package pluginfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionControlInboxAdmissionAndHeldRetrieval(t *testing.T) {
	x := newInboxReadFixture(t, "")
	if err := bus.SetSessionPlugin(x.f.store, x.from.ID, x.key, x.status.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := x.service.Send(context.Background(), x.request()); err == nil {
		t.Fatal("Z6_MUTED_INBOX_ADMISSION_DENIED")
	}
	if m, code := x.f.NextPluginInbox(&x.status); m != nil || code != "ok" {
		t.Fatal("Z6_MUTED_HELD_INBOX_HIDDEN", code)
	}
	if code := x.f.ConsumePluginInbox(&x.status, x.message.ID); code == "ok" {
		t.Fatal("Z6_MUTED_HELD_CONSUME_DENIED")
	}
	raw, err := x.f.ReadPluginInboxMessage(x.message.ID)
	if err == nil || len(raw) != 0 {
		t.Fatal("Z6_MUTED_OWNER_INBOX_DENIED")
	}
	if x.record(t, x.message.ID).AcknowledgedAt != "" {
		t.Fatal("Z6_PURE_DENIAL_DOES_NOT_CONSUME")
	}
	if err := bus.SetSessionPlugin(x.f.store, x.from.ID, x.key, x.status.Name, false); err != nil {
		t.Fatal(err)
	}
	if m := x.next(t); !x.isFirst(m) {
		t.Fatal("Z6_UNMUTE_RESTORES_HELD_INBOX")
	}
}

func TestSessionControlSendAndStoredReplyRetry(t *testing.T) {
	t.Run("send", func(t *testing.T) {
		x := newPluginSendFixture(t)
		if err := bus.SetSessionPlugin(x.store, x.granted.ID, x.key, x.status.Name, true); err != nil {
			t.Fatal(err)
		}
		if _, err := x.send("muted", x.granted.ID, "private"); err == nil {
			t.Fatal("Z6_MUTED_SEND_DENIED")
		}
		if x.messageCount(t) != 0 {
			t.Fatal("Z6_MUTED_SEND_NO_RESERVATION")
		}
	})
	t.Run("reply", func(t *testing.T) {
		x := newReplyFixture(t)
		before, err := x.reply("retry", x.message.ID, x.from.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := bus.SetSessionPlugin(x.f.store, x.from.ID, x.key, x.status.Name, true); err != nil {
			t.Fatal(err)
		}
		if _, err := x.reply("retry", x.message.ID, x.from.ID); err == nil {
			t.Fatal("Z6_MUTED_STORED_REPLY_RETRY_DENIED")
		}
		if err := bus.SetSessionPlugin(x.f.store, x.from.ID, x.key, x.status.Name, false); err != nil {
			t.Fatal(err)
		}
		after, err := x.reply("retry", x.message.ID, x.from.ID)
		if err != nil || after.ID != before.ID {
			t.Fatal("Z6_REPLY_RETRY_RETAINS_IDEMPOTENCE", err)
		}
	})
}

func TestSessionControlNativeAndOwnerRead(t *testing.T) {
	f, _, from := externalPinFixture(t)
	target, key := register(t, f.store, "cursor")
	message := model.Message{ID: model.NewUUID(), From: from, To: target.ID, TargetGeneration: target.Generation, Body: "private body", CreatedAt: model.Timestamp(), Status: model.StatusSubmitted}
	if err := saveTestMessage(f.store, message); err != nil {
		t.Fatal(err)
	}
	if err := f.GrantPluginAccess(PluginAccess{Status: true, Read: []string{target.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if raw, code, _ := f.ReadPluginMessageBound(&status, target.ID, message.ID); code != "ok" || len(raw) == 0 {
		t.Fatal("read positive", code)
	}
	if err := bus.SetSessionPlugin(f.store, target.ID, key, status.Name, true); err != nil {
		t.Fatal(err)
	}
	if raw, code, _ := f.ReadPluginMessageBound(&status, target.ID, message.ID); code == "ok" || bytes.Contains(raw, []byte(message.Body)) {
		t.Fatal("Z6_MUTED_NATIVE_READ_DENIED")
	}
	if raw, err := f.ReadPluginMessage(target.ID, message.ID); err == nil || len(raw) != 0 {
		t.Fatal("Z6_MUTED_OWNER_READ_DENIED")
	}
}

func TestSessionControlFinalDeliveryBinding(t *testing.T) {
	for _, change := range []string{"unchanged", "mute", "grant", "pin", "replacement", "missing-binding"} {
		t.Run(change, func(t *testing.T) {
			assertFinalSessionBinding(t, change)
		})
	}
}

func containsPrivateBinding(raw []byte, binding string) bool {
	return bytes.Contains(raw, []byte("plugin_binding")) || binding != "" && bytes.Contains(raw, []byte(binding))
}

func TestSessionControlFinalReplyHasNoSecondAdmission(t *testing.T) {
	x := newReplyFixture(t)
	message, err := x.reply("admitted", x.message.ID, x.from.ID)
	if err != nil {
		t.Fatal(err)
	}
	ledgerBefore, err := os.ReadFile(x.f.replyLedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	created, err := time.Parse(time.RFC3339Nano, x.message.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	atReplyTime(t, created.Add(replyWindow+time.Hour))
	if err := bus.WithJournalWrite(x.f.store, func(v bus.JournalView) error { return CheckPluginDelivery(v, x.f.store.Dir, message) }); err != nil {
		t.Fatal("Z6_ADMITTED_REPLY_HAS_NO_SECOND_TTL", err)
	}
	ledgerAfter, err := os.ReadFile(x.f.replyLedgerPath())
	if err != nil || !bytes.Equal(ledgerBefore, ledgerAfter) {
		t.Fatal("Z6_FINAL_REPLY_NO_COUNTER_CHANGE", err)
	}
}

func TestSessionControlReadPeerPolicy(t *testing.T) {
	f, _, _ := externalPinFixture(t)
	from, key := register(t, f.store, "cursor")
	target, _ := register(t, f.store, "cursor")
	message := model.Message{ID: model.NewUUID(), From: from.ID, To: target.ID, TargetGeneration: target.Generation, Body: "peer private body", CreatedAt: model.Timestamp(), Status: model.StatusSubmitted}
	if err := saveTestMessage(f.store, message); err != nil {
		t.Fatal(err)
	}
	if err := f.GrantPluginAccess(PluginAccess{Status: true, Read: []string{target.ID}}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if err := bus.SetSessionPlugin(f.store, from.ID, key, status.Name, true); err != nil {
		t.Fatal(err)
	}
	raw, code, _ := f.ReadPluginMessageBound(&status, target.ID, message.ID)
	if code == "ok" || bytes.Contains(raw, []byte(`"message":`)) || bytes.Contains(raw, []byte(message.Body)) {
		t.Fatal("Z6_NATIVE_READ_REQUIRES_PEER_POLICY", code)
	}
	raw, err = f.ReadPluginMessage(target.ID, message.ID)
	if err == nil || len(raw) != 0 {
		t.Fatal("Z6_OWNER_READ_REQUIRES_PEER_POLICY")
	}
}

func assertFinalSessionBinding(t *testing.T, change string) {
	t.Helper()
	x := newPluginSendFixture(t)
	m, err := x.send("pending", x.granted.ID, "private held body")
	if err != nil {
		t.Fatal(err)
	}
	if m.PluginBinding != x.status.Revision {
		t.Fatal("Z6_ORIGINAL_CHECKED_BINDING_PERSISTED")
	}
	switch change {
	case "mute":
		err = bus.SetSessionPlugin(x.store, x.granted.ID, x.key, x.status.Name, true)
	case "grant":
		err = x.f.GrantPluginAccess(PluginAccess{Status: true})
	case "pin":
		p, _, e := x.f.loadPlugin()
		if e != nil {
			t.Fatal(e)
		}
		p.Launch.PinSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		_, err = x.f.savePlugin(p)
	case "replacement":
		_, _, err = x.store.Register("cursor", x.granted.NativeID, x.granted.CWD, nil)
	case "missing-binding":
		m.PluginBinding = ""
	}
	if err != nil {
		t.Fatal(err)
	}
	err = bus.WithJournalWrite(x.store, func(v bus.JournalView) error { return CheckPluginDelivery(v, x.store.Dir, m) })
	if (err == nil) != (change == "unchanged") {
		t.Fatal("Z6_FINAL_CURRENT_BINDING_AND_POLICY", change, err)
	}
	if change == "missing-binding" {
		return
	}
	text, err := x.store.EmitPending(x.granted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if (text != "") != (change == "unchanged") {
		t.Fatal("Z6_FINAL_HOOK_HANDOFF_FENCED", change)
	}
	raw, _ := json.Marshal(m)
	if containsPrivateBinding(raw, m.PluginBinding) {
		t.Fatal("Z6_MESSAGE_BINDING_REDACTED")
	}
}

func TestSessionControlAllowListCannotWidenOwner(t *testing.T) {
	t.Setenv("ZALKI_PLUGINS", "external")
	x := newPluginSendFixture(t)
	if _, err := x.send("ungranted", x.foreign, "denied"); err == nil {
		t.Fatal("Z6_ALLOWLIST_CANNOT_WIDEN_OWNER_SEND")
	}
}

func TestSessionControlMuteNeverRebinds(t *testing.T) {
	t.Setenv("ZALKI_PLUGINS", "")
	x := newPluginSendFixture(t)
	t.Setenv("ZALKI_PLUGINS", "off")
	if err := bus.SetSessionPlugin(x.store, x.granted.ID, x.key, x.status.Name, true); err != nil {
		t.Fatal(err)
	}
	after, err := x.f.PluginStatus()
	if err != nil || after.ID != x.status.ID || after.Revision != x.status.Revision || after.Epoch != x.status.Epoch {
		t.Fatal("Z6_MUTE_NEVER_REBINDS", err)
	}
}

func TestSessionControlPendingQueriesFenceReplacement(t *testing.T) {
	x := newPluginSendFixture(t)
	m, err := x.send("old-registration", x.granted.ID, "private pending old registration")
	if err != nil {
		t.Fatal(err)
	}
	current, key, err := x.store.Register("cursor", x.granted.NativeID, x.granted.CWD, nil)
	if err != nil {
		t.Fatal(err)
	}
	query := bus.MessageQuery{Actor: current.ID, Capability: key, History: true, IncludeBody: true}
	page, err := x.store.ReadMessages(query)
	if err != nil || len(page.Messages) != 0 {
		t.Fatal("Z6_REPLACED_PENDING_PLUGIN_NOT_NEW_EXPOSURE", err)
	}
	query.MessageID = m.ID
	if _, err := x.store.ReadMessages(query); err == nil {
		t.Fatal("Z6_REPLACED_PENDING_EXACT_NOT_NEW_EXPOSURE")
	}
}

func TestSessionControlAllowListCannotWidenStandardReceive(t *testing.T) {
	t.Setenv("ZALKI_PLUGINS", "external")
	x := newInboxReadFixture(t, "")
	if err := x.f.ConfigurePluginScope("none"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.service.Send(t.Context(), x.request()); err == nil {
		t.Fatal("Z6_ALLOWLIST_CANNOT_WIDEN_OWNER_RECEIVE")
	}
}
