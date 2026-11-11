package bus

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInboxAdmissionRequiresLiveWriteScope(t *testing.T) {
	for _, kind := range []string{"zero", "expired", "foreign", "read", "future read"} {
		t.Run(kind, func(t *testing.T) { checkPluginInboxAdmissionScope(t, kind) })
	}
}

func checkPluginInboxAdmissionScope(t *testing.T, kind string) {
	t.Helper()
	x := newInboxStorageFixture(t)
	if kind == "future read" {
		futureStateMarker(t, x.f.store)
		if model.ErrorCodeOf(x.f.store.CheckWriteCompatibility()) != model.CodeUnsupportedSchema {
			t.Fatal("fixture must fence writers")
		}
	}
	before := journalSnapshot(t, x.f.store)
	check := func(view JournalView) error {
		var denial error
		for _, to := range []string{"plugin:" + x.status.ID, "plugin:invalid"} {
			reserve, err := AdmitPluginInbox(view, x.f.store.Dir, to, x.from.ID, x.from.Generation)
			if reserve != nil || model.ErrorCodeOf(err) != model.CodeUnauthorized {
				t.Fatal("invalid scope must deny admission with a typed error", err)
			}
			if err.Error() != "unauthorized: plugin write requires its live journal scope" {
				t.Fatal("scope denial must precede destination lookup", err)
			}
			denial = err
		}
		return denial
	}
	switch kind {
	case "zero":
		_ = check(JournalView{})
	case "expired":
		var retained JournalView
		if err := WithJournalWrite(x.f.store, func(view JournalView) error {
			retained = view
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_ = check(retained)
	case "foreign":
		locked := fixtureStore(t)
		if err := WithJournalWrite(locked, func(view JournalView) error {
			assertJournalLock(t, locked, true)
			assertJournalLock(t, x.f.store, false)
			return check(view)
		}); model.ErrorCodeOf(err) != model.CodeUnauthorized {
			t.Fatal(err)
		}
	default:
		if err := WithJournalRead(x.f.store, check); model.ErrorCodeOf(err) != model.CodeUnauthorized {
			t.Fatal("read callback must run and propagate the scope denial", err)
		}
	}
	assertPluginInboxScopeUnchanged(t, x, before)
}

func TestPluginInboxRetainedReservationCannotWrite(t *testing.T) {
	for _, kind := range []string{"missing", "empty", "populated"} {
		t.Run(kind, func(t *testing.T) { checkPluginInboxRetainedReservation(t, kind) })
	}
}

func checkPluginInboxRetainedReservation(t *testing.T, kind string) {
	t.Helper()
	x := newInboxStorageFixture(t)
	switch kind {
	case "empty":
		now := time.Now().UTC()
		x.putIndex(t, pluginInboxIndex{Version: 1, Installation: x.status.ID, Entries: []pluginInboxEntry{}, Quota: model.NewFloodState(now)})
	case "populated":
		if _, err := x.service.Send(t.Context(), x.request()); err != nil {
			t.Fatal(err)
		}
	}
	var reserve func(model.Message, time.Time) error
	var retained JournalView
	if err := WithJournalWrite(x.f.store, func(view JournalView) error {
		retained = view
		var err error
		reserve, err = AdmitPluginInbox(view, x.f.store.Dir, "plugin:"+x.status.ID, x.from.ID, x.from.Generation)
		return err
	}); err != nil || reserve == nil {
		t.Fatal("live originating write scope must admit", err)
	}
	assertInactiveJournalView(t, retained)
	assertJournalLock(t, x.f.store, false)
	before := journalSnapshot(t, x.f.store)
	now := time.Now().UTC()
	if err := reserve(pluginInboxScopeMessage(x, now), now); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("retained reservation must return a typed scope error", err)
	}
	if after := journalSnapshot(t, x.f.store); !reflect.DeepEqual(before, after) {
		t.Fatal("retained reservation changed persisted state")
	}
	if kind == "missing" {
		assertPluginInboxScopeUnchanged(t, x, before)
	}
}

func pluginInboxScopeMessage(x inboxStorageFixture, now time.Time) model.Message {
	r := x.request()
	at := now.Format(time.RFC3339Nano)
	return model.Message{Schema: recordSchema, ID: r.MessageID, From: x.from.ID, To: r.To, TargetGeneration: x.status.ID, Body: r.Body, CreatedAt: at, Status: model.StatusSubmitted, Receipt: &model.Receipt{At: at, Status: model.StatusSubmitted}}
}

func assertPluginInboxScopeUnchanged(t *testing.T, x inboxStorageFixture, before map[string]string) {
	t.Helper()
	if after := journalSnapshot(t, x.f.store); !reflect.DeepEqual(before, after) {
		t.Fatal("invalid scope changed persisted state")
	}
	if _, err := os.Stat(filepath.Join(x.f.pluginDataPath(), "inbox.json")); !os.IsNotExist(err) {
		t.Fatal("invalid scope created an inbox index", err)
	}
}
