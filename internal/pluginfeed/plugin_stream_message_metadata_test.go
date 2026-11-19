package pluginfeed

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamMessageMetadataVisibilityAndStableDeliveryUpdates(t *testing.T) {
	for _, visible := range []string{"both", "from", "to", "neither"} {
		t.Run(visible, func(t *testing.T) { checkStreamMessageVisibility(t, visible) })
	}
}

func checkStreamMessageVisibility(t *testing.T, visible string) {
	t.Helper()
	store := fixtureStore(t)
	from, fromKey := register(t, store, "codex")
	to, toKey := register(t, store, "cursor")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	if visible == "to" || visible == "neither" {
		if err := bus.SetSessionPlugin(store, from.ID, fromKey, feed.name, true); err != nil {
			t.Fatal(err)
		}
	}
	if visible == "from" || visible == "neither" {
		if err := bus.SetSessionPlugin(store, to.ID, toKey, feed.name, true); err != nil {
			t.Fatal(err)
		}
	}
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	if visible != "to" && visible != "neither" {
		primeStreamSession(t, store, feed, launch, from.ID)
	}
	if visible != "from" && visible != "neither" {
		primeStreamSession(t, store, feed, launch, to.ID)
	}
	body := "F_METADATA_BODY_SENTINEL_<>&_PRIVATE"
	message := createStreamMetadataFacts(t, store, from.ID, to.ID, fromKey, body)
	if visible == "neither" {
		if d := streamInspect(t, feed, launch); d.ID != "" || len(d.Frames) != 0 {
			t.Fatal("F_METADATA_BOTH_HIDDEN_NO_EVENT")
		}
		return
	}
	expectedFrom, expectedTo := streamMetadataAddresses(visible, from.ID, to.ID)
	want := streamMetadataExpected{messageID: message.ID, from: expectedFrom, to: expectedTo, secrets: []string{body, fromKey, toKey, store.Dir, "plugin_policy", "plugin_binding", "preview", "body_size", "receipt"}}
	assertStreamMessageUpdates(t, feed, launch, want)
}

func assertStreamMessageUpdates(t *testing.T, feed PluginFeed, launch *PluginFeedStatus, want streamMetadataExpected) {
	t.Helper()
	for index, status := range []model.DeliveryStatus{model.StatusAttempting, model.StatusSubmitted} {
		delivery, frame := streamTake(t, feed, launch)
		want.status = status
		if index == 1 {
			want.previous = model.StatusAttempting
		}
		assertStreamMetadataFact(t, frame, want)
		if _, err := feed.AckPluginFeed(launch, delivery.ID); err != nil {
			t.Fatal(err)
		}
		want.priorEvent = frame.ID
	}
}

type streamMetadataExpected struct {
	messageID, priorEvent, from, to string
	status, previous                model.DeliveryStatus
	secrets                         []string
}

func assertStreamMetadataFact(t *testing.T, frame streamFrame, want streamMetadataExpected) {
	t.Helper()
	if frame.Event == nil || frame.Event.Message == nil {
		t.Fatal("F_METADATA_REAL_MESSAGE_EVENT")
	}
	got := frame.Event.Message
	assertStreamMetadataIdentity(t, frame, want)
	if got.From != want.from || got.To != want.to {
		t.Fatal("F_METADATA_ENDPOINT_KIND_ONLY_REDACTION", got)
	}
	if got.Previous != want.previous {
		t.Fatal("F_METADATA_PREVIOUS_ONLY_AFTER_ACK", got)
	}
	raw, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range want.secrets {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatal("F_METADATA_BODY_CREDENTIAL_PATH_FREE", secret)
		}
	}
}

func assertStreamMetadataIdentity(t *testing.T, frame streamFrame, want streamMetadataExpected) {
	t.Helper()
	got := frame.Event.Message
	if got.ID != want.messageID || frame.ID != frame.Event.ID || frame.ID == want.priorEvent || !model.IsCanonicalUUID(frame.ID) || frame.Event.Type != model.EventMessageStatusChanged || frame.Event.OccurredAt == "" || got.Status != want.status {
		t.Fatal("F_METADATA_STABLE_MESSAGE_DISTINCT_EVENT_AND_STATUS", frame)
	}
}

func streamMetadataAddresses(visible, from, to string) (string, string) {
	if visible == "to" {
		from = "codex:"
	}
	if visible == "from" {
		to = "cursor:"
	}
	return from, to
}

func createStreamMetadataFacts(t *testing.T, store bus.Store, from, to, key, body string) model.Message {
	t.Helper()
	message, _, _, err := store.ReserveMessage(bus.SendRequest{Actor: from, Capability: key, To: to, MessageID: model.NewUUID(), Body: body})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(message.ID, model.Receipt{At: model.Timestamp(), Status: model.StatusSubmitted}); err != nil {
		t.Fatal(err)
	}
	// Source metadata is sufficient; a body fetch would fail this production path.
	if err := os.Remove(filepath.Join(store.Dir, "messages", message.ID+".json")); err != nil {
		t.Fatal(err)
	}
	return message
}
