package bus

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginInboxMissingPortDeniesAfterNativePrechecks(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.service.PluginInbox = nil
	before := journalSnapshot(t, x.f.store)
	for _, test := range []struct {
		name   string
		edit   func(*SendRequest)
		code   model.ErrorCode
		detail string
	}{
		{"fresh", func(*SendRequest) {}, model.CodeUnauthorized, "plugin inbox unavailable"},
		{"invalid ID", func(r *SendRequest) { r.MessageID = "wrong" }, model.CodeInvalidIdentity, "expected an exact"},
		{"wrong capability", func(r *SendRequest) { r.Capability = "wrong" }, model.CodeUnauthorized, "wrong session capability"},
		{"reply", func(r *SendRequest) { r.ReplyTo = newID() }, model.CodeInvalidInput, "plugin inbox replies are unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := x.request()
			test.edit(&request)
			message, err := x.service.Send(t.Context(), request)
			assertInboxFailure(t, message, err)
			if model.ErrorCodeOf(err) != test.code || !strings.Contains(err.Error(), test.detail) {
				t.Fatal("plugin denial changed native precheck order", err)
			}
		})
	}
	if after := journalSnapshot(t, x.f.store); !reflect.DeepEqual(before, after) {
		t.Fatal("an absent port modified persisted state")
	}
}

func TestPluginInboxExactRetryAuthenticatesBeforeMissingPort(t *testing.T) {
	x := newInboxStorageFixture(t)
	request := x.request()
	message, err := x.service.Send(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	x.service.PluginInbox = nil
	before := journalSnapshot(t, x.f.store)
	if again, err := x.service.Send(t.Context(), request); err != nil || !reflect.DeepEqual(again, message) {
		t.Fatal("exact retry was incorrectly routed through admission", err)
	}
	request.Capability = "wrong"
	again, err := x.service.Send(t.Context(), request)
	assertInboxFailure(t, again, err)
	if model.ErrorCodeOf(err) != model.CodeUnauthorized || !strings.Contains(err.Error(), "wrong session capability") {
		t.Fatal("wrong capability disclosed an exact retry", err)
	}
	if after := journalSnapshot(t, x.f.store); !reflect.DeepEqual(before, after) {
		t.Fatal("retry changed persisted bytes")
	}
}

func TestPluginInboxNilReservationDeniesBeforePublication(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.service.PluginInbox = func(JournalView, string, string, string, string) (func(model.Message, time.Time) error, error) {
		return nil, nil
	}
	before := journalSnapshot(t, x.f.store)
	message, err := x.service.Send(t.Context(), x.request())
	assertInboxFailure(t, message, err)
	if model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("missing reservation must deny admission", err)
	}
	if after := journalSnapshot(t, x.f.store); !reflect.DeepEqual(before, after) {
		t.Fatal("missing reservation modified persisted state")
	}
}

func TestPluginInboxAdmissionAndReservationUseTheLockedStore(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.service.PluginInbox = func(view JournalView, stateDir, to, sender, generation string) (func(model.Message, time.Time) error, error) {
		if stateDir != x.f.store.Dir || sender != x.from.ID || generation != x.from.Generation {
			t.Fatal("admission got the wrong journal or sender")
		}
		assertJournalLock(t, x.f.store, true)
		reserve, err := AdmitPluginInbox(view, stateDir, to, sender, generation)
		if err != nil {
			return nil, err
		}
		return func(message model.Message, now time.Time) error {
			assertJournalLock(t, x.f.store, true)
			return reserve(message, now)
		}, nil
	}
	if _, err := x.service.Send(t.Context(), x.request()); err != nil {
		t.Fatal(err)
	}
	assertJournalLock(t, x.f.store, false)
}
