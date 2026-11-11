package bus_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestConfiguredBodyPluginAdmissionOrdering(t *testing.T) {
	x := newInboxAdmissionFixture(t)
	admit := x.service.PluginInbox
	var calls int
	x.service.PluginInbox = func(view bus.JournalView, dir, to, sender, generation string) (func(model.Message, time.Time) error, error) {
		calls++
		return admit(view, dir, to, sender, generation)
	}
	request := x.request()
	request.Body = "12345678"
	x.service.Store.MaxBodyBytes = 4
	before := pluginScopeSnapshot(t, x.store)
	msg, err := x.service.Send(t.Context(), request)
	assertInboxFailure(t, msg, err)
	if model.ErrorCodeOf(err) != model.CodeInvalidInput || calls != 0 || !reflect.DeepEqual(before, pluginScopeSnapshot(t, x.store)) {
		t.Fatal("plugin oversize admission called the reservation policy or changed quota/markers", err, calls)
	}
	x.service.Store.MaxBodyBytes = model.MaxMessageBodyBytes
	first, err := x.service.Send(t.Context(), request)
	if err != nil || calls != 1 {
		t.Fatal("initial plugin send", err, calls)
	}
	size, err := bus.EncodedMessageSize(first)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(x.store.Dir, "messages", first.ID+".json"))
	if err != nil || len(raw) != size {
		t.Fatal("plugin size calculation differs from the writer", err, len(raw), size)
	}
	x.service.Store.MaxBodyBytes = 4
	before = pluginScopeSnapshot(t, x.store)
	again, err := x.service.Send(t.Context(), request)
	if err != nil || again.ID != first.ID || calls != 1 {
		t.Fatal("plugin committed retry reapplied admission", err, calls)
	}
	request.Body += "!"
	if _, err := x.service.Send(t.Context(), request); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("plugin conflict was hidden by the new cap", err)
	}
	if calls != 1 || !reflect.DeepEqual(before, pluginScopeSnapshot(t, x.store)) {
		t.Fatal("plugin retry/conflict changed its index, quota or markers")
	}
}
