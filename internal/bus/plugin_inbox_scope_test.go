package bus_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func TestPluginInboxAdmissionRequiresLiveWriteScope(t *testing.T) {
	for _, kind := range []string{"zero", "expired", "foreign", "read", "future read"} {
		t.Run(kind, func(t *testing.T) { checkPluginInboxAdmissionScope(t, kind) })
	}
}

func checkPluginInboxAdmissionScope(t *testing.T, kind string) {
	t.Helper()
	x := newInboxAdmissionFixture(t)
	if kind == "future read" {
		pluginScopeFutureMarker(t, x.store)
		if model.ErrorCodeOf(x.store.CheckWriteCompatibility()) != model.CodeUnsupportedSchema {
			t.Fatal("fixture must fence writers")
		}
	}
	before := pluginScopeSnapshot(t, x.store)
	check := func(view bus.JournalView) error {
		var denial error
		for _, to := range []string{"plugin:" + x.status.ID, "plugin:invalid"} {
			reserve, err := pluginfeed.AdmitPluginInbox(view, x.store.Dir, to, x.from.ID, x.from.Generation)
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
		_ = check(bus.JournalView{})
	case "expired":
		var retained bus.JournalView
		if err := bus.WithJournalWrite(x.store, func(view bus.JournalView) error {
			retained = view
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		_ = check(retained)
	case "foreign":
		locked := newPluginScopeStore(t)
		if err := bus.WithJournalWrite(locked, func(view bus.JournalView) error {
			assertPluginScopeLock(t, locked, true)
			assertPluginScopeLock(t, x.store, false)
			return check(view)
		}); model.ErrorCodeOf(err) != model.CodeUnauthorized {
			t.Fatal(err)
		}
	default:
		if err := bus.WithJournalRead(x.store, check); model.ErrorCodeOf(err) != model.CodeUnauthorized {
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
	x := newInboxAdmissionFixture(t)
	switch kind {
	case "empty":
		now := time.Now().UTC()
		x.putEmptyIndex(t, now)
	case "populated":
		if _, err := x.service.Send(t.Context(), x.request()); err != nil {
			t.Fatal(err)
		}
	}
	var reserve func(model.Message, time.Time) error
	var retained bus.JournalView
	if err := bus.WithJournalWrite(x.store, func(view bus.JournalView) error {
		retained = view
		var err error
		reserve, err = pluginfeed.AdmitPluginInbox(view, x.store.Dir, "plugin:"+x.status.ID, x.from.ID, x.from.Generation)
		return err
	}); err != nil || reserve == nil {
		t.Fatal("live originating write scope must admit", err)
	}
	assertPluginViewInactive(t, retained)
	assertPluginScopeLock(t, x.store, false)
	before := pluginScopeSnapshot(t, x.store)
	now := time.Now().UTC()
	if err := reserve(pluginInboxScopeMessage(x, now), now); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("retained reservation must return a typed scope error", err)
	}
	if after := pluginScopeSnapshot(t, x.store); !reflect.DeepEqual(before, after) {
		t.Fatal("retained reservation changed persisted state")
	}
	if kind == "missing" {
		assertPluginInboxScopeUnchanged(t, x, before)
	}
}

func pluginInboxScopeMessage(x inboxAdmissionFixture, now time.Time) model.Message {
	r := x.request()
	at := now.Format(time.RFC3339Nano)
	return model.Message{Schema: 1, ID: r.MessageID, From: x.from.ID, To: r.To, TargetGeneration: x.status.ID, Body: r.Body, CreatedAt: at, Status: model.StatusSubmitted, Receipt: &model.Receipt{At: at, Status: model.StatusSubmitted}}
}

func assertPluginInboxScopeUnchanged(t *testing.T, x inboxAdmissionFixture, before map[string]string) {
	t.Helper()
	if after := pluginScopeSnapshot(t, x.store); !reflect.DeepEqual(before, after) {
		t.Fatal("invalid scope changed persisted state")
	}
	if _, err := os.Stat(filepath.Join(x.status.DataDir, "inbox.json")); !os.IsNotExist(err) {
		t.Fatal("invalid scope created an inbox index", err)
	}
}
