package bus

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func limitSnapshot(t *testing.T, store Store) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(store.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			files[path] = string(raw)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestConfiguredBodyRefusalHasNoEffects(t *testing.T) {
	for _, state := range []model.SessionState{model.StateReady, model.StateApproval} {
		t.Run(string(state), func(t *testing.T) {
			store := fixtureStore(t)
			from, key, to, _ := registerPair(t, store, "cursor", "codex")
			if err := store.Update(to.ID, state, false); err != nil {
				t.Fatal(err)
			}
			store.MaxBodyBytes = 4
			var delivered []string
			service := approvalService(t, store, &delivered)
			before := limitSnapshot(t, store)
			msg, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "ééé"})
			if model.ErrorCodeOf(err) != model.CodeInvalidInput || err.Error() != "invalid_input: body is 6 UTF-8 bytes; configured maximum is 4 bytes" || msg.ID != "" || len(delivered) != 0 {
				t.Fatalf("wrong configured refusal: %+v %v", msg, err)
			}
			if !reflect.DeepEqual(before, limitSnapshot(t, store)) {
				t.Fatal("oversize refusal changed quota, holds, records or markers")
			}
		})
	}
}

func TestConfiguredBodyDurableSendAndHistory(t *testing.T) {
	store := fixtureStore(t)
	from, key, to, toKey := registerPair(t, store, "cursor", "codex")
	var delivered []string
	service := approvalService(t, store, &delivered)
	request := SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "12345678"}
	first, err := service.Send(t.Context(), request)
	if err != nil || len(delivered) != 1 {
		t.Fatal("initial send", err)
	}
	service.Store.MaxBodyBytes = 4
	before := limitSnapshot(t, store)
	again, err := service.Send(t.Context(), request)
	if err != nil || again.ID != first.ID || again.Status != first.Status || len(delivered) != 1 {
		t.Fatal("lower cap rejected or resent a committed message", err)
	}
	request.Body += "!"
	if _, err := service.Send(t.Context(), request); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatalf("cap hid an existing-ID conflict: %v", err)
	}
	if !reflect.DeepEqual(before, limitSnapshot(t, store)) {
		t.Fatal("retry/conflict changed durable state")
	}
	batch, err := service.Store.ReadMessages(MessageQuery{Actor: from.ID, Capability: key, History: true, Sent: true, IncludeBody: true})
	if err != nil || len(batch.Messages) != 1 || batch.Messages[0].Body != first.Body {
		t.Fatal("lower cap hid historical content", err)
	}
	if _, err := service.Store.Ack(to.ID, toKey, first.ID); err != nil {
		t.Fatal("lower cap blocked settlement", err)
	}
}

func TestConfiguredBodyReplyRefusalAndRetry(t *testing.T) {
	store := fixtureStore(t)
	from, key, to, toKey := registerPair(t, store, "codex", "codex")
	var delivered []string
	service := approvalService(t, store, &delivered)
	parent, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	request := ReplyRequest{Actor: to.ID, Capability: toKey, MessageID: parent.ID, Body: "12345678"}
	service.Store.MaxBodyBytes = 4
	before := limitSnapshot(t, store)
	if _, err := service.Reply(t.Context(), request); model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatal("oversize reply accepted", err)
	}
	if !reflect.DeepEqual(before, limitSnapshot(t, store)) || len(delivered) != 1 {
		t.Fatal("oversize reply ACKed, charged quota, reserved or wrote a marker")
	}
	service.Store.MaxBodyBytes = model.MaxMessageBodyBytes
	first, err := service.Reply(t.Context(), request)
	if err != nil || len(delivered) != 2 {
		t.Fatal("initial reply", err)
	}
	service.Store.MaxBodyBytes = 4
	before = limitSnapshot(t, store)
	again, err := service.Reply(t.Context(), request)
	if err != nil || again.ID != first.ID || len(delivered) != 2 {
		t.Fatal("lower cap rejected or resent a committed reply", err)
	}
	if !reflect.DeepEqual(before, limitSnapshot(t, store)) {
		t.Fatal("committed reply retry changed durable state")
	}
}

func TestConfiguredBodyQueueReleaseKeepsHardLimit(t *testing.T) {
	store := fixtureStore(t)
	from, key, to, _ := registerPair(t, store, "cursor", "codex")
	if err := store.Update(to.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	request := SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "12345678"}
	queued, err := service.Send(t.Context(), request)
	if err != nil || !queued.Held || len(delivered) != 0 {
		t.Fatal("initial queue reservation", err)
	}
	marker := filepath.Join(store.Dir, openMessagesDir, queued.ID)
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("queued message has no marker", err)
	}
	service.Store.MaxBodyBytes = 4
	if _, err := service.Send(t.Context(), request); err != nil {
		t.Fatal("lower cap rejected a durable hold", err)
	}
	stopHook(t, service, "codex", to.ID)
	saved, err := store.message(queued.ID)
	if err != nil || saved.Status != model.StatusSubmitted || len(delivered) != 1 {
		t.Fatal("lower cap blocked queued release", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("settled queue marker remained", err)
	}
}

func TestConfiguredBodyDifferingSnapshots(t *testing.T) {
	store := fixtureStore(t)
	from, key, to, _ := registerPair(t, store, "cursor", "codex")
	var calls atomic.Int32
	high := Service{Store: store, Deliver: func(_ context.Context, _ model.Message, _ model.Session) model.Receipt {
		calls.Add(1)
		return model.NewReceipt(model.StatusSubmitted)
	}}
	high.Store.MaxBodyBytes = 8
	low := high
	low.Store.MaxBodyBytes = 4
	results := make(chan error, 2)
	for _, service := range []Service{high, low} {
		go func(service Service) {
			_, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "12345678"})
			results <- err
		}(service)
	}
	var accepted, refused int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			accepted++
		case model.ErrorCodeOf(err) == model.CodeInvalidInput:
			refused++
		default:
			t.Error(err)
		}
	}
	if accepted != 1 || refused != 1 || calls.Load() != 1 {
		t.Fatal("snapshots did not enforce independent admission", accepted, refused, calls.Load())
	}
}

func TestConfiguredBodyLimitDefaultsAndBounds(t *testing.T) {
	if err := checkAdmissionBody(strings.Repeat("x", model.MaxMessageBodyBytes), 0); err != nil {
		t.Fatal("zero-value Store lost the hard default", err)
	}
	for _, limit := range []int{-1, model.MaxMessageBodyBytes + 1} {
		if err := checkAdmissionBody("x", limit); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatal("invalid injected limit accepted", err)
		}
	}
}
