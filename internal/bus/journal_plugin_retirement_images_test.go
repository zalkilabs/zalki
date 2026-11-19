package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/zalkilabs/zalki/internal/model"
	"os"
	"testing"
)

func TestFeedMemberRetirementWitnessBindsExactImages(t *testing.T) {
	store, session, _, _ := memberCacheFixture(t)
	path, err := store.sessionPath(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(session.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := collectMembers(t, store, []string{session.ID}, &memberWorkCount{})
	if err != nil || len(rows.Records) != 1 {
		t.Fatal(err)
	}
	assertRetirementImages(t, store, rows.Records[0], before, after)
}
func assertRetirementImages(t *testing.T, store Store, row pluginNativeProjection, before, after []byte) {
	t.Helper()
	proof := row.Retirement
	if proof == nil {
		t.Fatal("F_RETIREMENT_IMAGE_PROOF_PRESENT")
		return
	}
	prior := sha256.Sum256(before)
	next := sha256.Sum256(after)
	if proof.BeforeDigest != hex.EncodeToString(prior[:]) {
		t.Fatal("F_RETIREMENT_SOURCE_PREIMAGE_DIGEST_EXACT")
	}
	if proof.Digest != hex.EncodeToString(next[:]) || proof.Digest != row.Digest || proof.BeforeDigest == proof.Digest {
		t.Fatal("F_RETIREMENT_SOURCE_AFTERIMAGE_DIGEST_EXACT")
	}
	var event model.Event
	err := WithJournalRead(store, func(view JournalView) error {
		head, err := view.EventWindow()
		if err != nil {
			return err
		}
		event, err = view.EventAt(head.Epoch, head.Last)
		return err
	})
	if err != nil || proof.Event != event.ID || event.Session.ID != row.ID || !event.Session.Retired {
		t.Fatal("F_RETIREMENT_EXACT_IMAGES_BIND_IMMUTABLE_EVENT", err)
	}
}
