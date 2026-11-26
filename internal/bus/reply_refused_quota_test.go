package bus

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// A refused reply is charged only when its record does not exist yet. Through
// Reply this cannot be seen: prepareMessage returns a record that exists before
// any refusal is made (reply_retry_quota_test.go pins that), so the charge is
// called directly in these tests.

// chargeRefused is the only line that names the code under test.
func chargeRefused(store Store, msg *model.Message) error {
	return store.locked(func() error { return replies(store).chargeRefusedReply(msg) })
}

// refusedReplyToCharge returns a reply to a retired session, settled as not
// sent as refuseReply does, and the path of the flood state of that session.
func refusedReplyToCharge(t *testing.T) (Store, model.Message, string) {
	t.Helper()
	store := fixtureStore(t)
	if err := store.locked(func() error { return nil }); err != nil { // creates the record folders
		t.Fatal(err)
	}
	actor, _, target, _ := registerPair(t, store, "cursor", "codex")
	parent := model.Message{ID: newID(), From: target.ID, To: actor.ID, Body: "review", Status: model.StatusSubmitted}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	msg, err := store.outgoingMessage(actor, target, newID(), "reply", parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := msg.SettleNotSent(model.CodeUnavailable, "destination has retired"); err != nil {
		t.Fatal(err)
	}
	path, err := store.floodPath(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, msg, path
}

func requireNoQuotaTaken(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("quota was taken", err)
	}
}

func TestChargeOfARefusedReplyTakesQuotaForANewRecord(t *testing.T) {
	store, msg, path := refusedReplyToCharge(t)
	if err := chargeRefused(store, &msg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("quota was not taken", err)
	}
	if msg.Receipt == nil || msg.Receipt.Code != model.CodeUnavailable {
		t.Fatalf("a charged refusal keeps its own refusal: %+v", msg.Receipt)
	}
}

func TestChargeOfARefusedReplyTakesNoQuotaForAnExistingRecord(t *testing.T) {
	store, msg, path := refusedReplyToCharge(t)
	if err := store.saveMessage(msg); err != nil {
		t.Fatal(err)
	}
	if err := chargeRefused(store, &msg); err != nil {
		t.Fatal(err)
	}
	requireNoQuotaTaken(t, path)
}

func TestChargeOfARefusedReplyFailsForARecordThatCannotBeRead(t *testing.T) {
	store, msg, path := refusedReplyToCharge(t)
	if err := os.Mkdir(filepath.Join(store.dir, "messages", msg.ID+".json"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := chargeRefused(store, &msg); err == nil {
		t.Fatal("a record that cannot be read was treated as missing")
	}
	requireNoQuotaTaken(t, path)
	if msg.Receipt == nil || msg.Receipt.Status != model.StatusNotSent {
		t.Fatalf("the failed charge must settle the message: %+v", msg.Receipt)
	}
}

func TestChargeOfARefusedReplySettlesTheMessageWhenTheQuotaIsUsedUp(t *testing.T) {
	store, msg, _ := refusedReplyToCharge(t)
	for range 12 {
		if err := spendFlood(store, model.Message{ID: newID(), From: msg.From, To: msg.To}, time.Now().Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	err := chargeRefused(store, &msg)
	if model.ErrorCodeOf(err) != model.CodeRateLimited || msg.Receipt == nil || msg.Receipt.Code != model.CodeRateLimited {
		t.Fatalf("a refused reply must not bypass quota: %+v %v", msg.Receipt, err)
	}
}
