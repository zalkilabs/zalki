package bus_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

type inboxAdmissionFixture struct {
	service bus.Service
	store   bus.Store
	status  pluginfeed.PluginFeedStatus
	from    model.Session
	key     string
}

func newInboxAdmissionFixture(t *testing.T) inboxAdmissionFixture {
	t.Helper()
	x := newInboxPublicFixture(t)
	status, err := x.feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	x.service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		t.Error("plugin called native transport")
		return model.NewReceipt(model.StatusSubmitted)
	}
	return inboxAdmissionFixture{*x.service, x.service.Store, status, x.author.Session, x.author.Key}
}

func (x inboxAdmissionFixture) request() bus.SendRequest {
	return bus.SendRequest{Actor: x.from.ID, Capability: x.key, To: "plugin:" + x.status.ID, MessageID: model.NewUUID(), Body: "STORAGE-PRIVATE-PAYLOAD"}
}

func (x inboxAdmissionFixture) putEmptyIndex(t *testing.T, now time.Time) {
	t.Helper()
	raw, err := journal.EncodeJSON(map[string]any{
		"version": 1, "installation": x.status.ID, "entries": []any{}, "quota": model.NewFloodState(now),
	}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(x.status.DataDir, "inbox.json"), raw); err != nil {
		t.Fatal(err)
	}
}

func newPluginScopeStore(t *testing.T) bus.Store {
	t.Helper()
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assertInboxFailure(t *testing.T, message model.Message, err error) {
	t.Helper()
	if err == nil || !reflect.DeepEqual(message, model.Message{}) {
		t.Fatal("failed admission returned message data", err)
	}
}

func pluginScopeSnapshot(t *testing.T, store bus.Store) map[string]string {
	t.Helper()
	snapshot := map[string]string{}
	if err := filepath.WalkDir(store.Dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(store.Dir, path)
		if err == nil {
			snapshot[rel] = string(raw)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func pluginScopeFutureMarker(t *testing.T, store bus.Store) {
	t.Helper()
	raw, err := journal.EncodeJSON(map[string]any{"schema": 1, "formats": map[string]int{"sessions": 3}}, bus.MaxMessageRecordBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(store.Dir, "state-formats.json"), raw); err != nil {
		t.Fatal(err)
	}
}

func assertPluginScopeLock(t *testing.T, store bus.Store, held bool) {
	t.Helper()
	fd, err := journal.OpenLockFile(filepath.Join(store.Dir, "lock"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.CloseLockFile(fd)
	err = journal.LockExclusive(fd, true)
	if (held && !errors.Is(err, syscall.EWOULDBLOCK)) || (!held && err != nil) {
		t.Fatalf("journal lock held=%v: probe returned %v", held, err)
	}
}

func assertPluginViewInactive(t *testing.T, view bus.JournalView) {
	t.Helper()
	if _, err := view.EventWindow(); err == nil {
		t.Fatal("retained view must expire")
	}
	if err := view.WithWriteScope("", func() error {
		t.Error("expired scope permitted IO")
		return nil
	}); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("expired write must have a typed error", err)
	}
}
