package bus

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestUnversionedRecordsAreRejectedWithoutMigration(t *testing.T) {
	for _, family := range []string{"session", "message", "watch"} {
		t.Run(family, func(t *testing.T) {
			for _, header := range []string{"zero", "missing"} {
				t.Run(header, func(t *testing.T) { checkUnversionedRead(t, family, header) })
			}
		})
	}
}

func checkUnversionedRead(t *testing.T, family, header string) {
	t.Helper()
	path, read := unversionedReadFixture(t, family)
	var record map[string]any
	if err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	if header == "missing" {
		delete(record, "schema")
	} else {
		record["schema"] = 0
	}
	if err := writeJSON(path, record); err != nil {
		t.Fatal(err)
	}
	before := readCommitFile(t, path)
	err := read()
	kind := "journal"
	if family == "session" {
		kind = "session"
	}
	if model.ErrorCodeOf(err) != model.CodeUnsupportedSchema || err.Error() != "unsupported_schema: unsupported "+kind+" schema 0" {
		t.Fatal("unversioned record did not fail with its unsupported-format error", err)
	}
	if after := readCommitFile(t, path); !bytes.Equal(before, after) {
		t.Fatal("rejection changed or migrated the record")
	}
}

func unversionedReadFixture(t *testing.T, family string) (path string, read func() error) {
	t.Helper()
	store := fixtureStore(t)
	target, _ := register(t, store, "codex")
	var err error
	switch family {
	case "session":
		path, err = store.sessionPath(target.ID)
		read = func() error { _, err := store.session(target.ID); return err }
	case "message":
		message := model.Message{ID: newID(), From: target.ID, To: target.ID, Body: "fixture", Status: model.StatusSubmitted}
		if err := store.saveMessage(message); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(store.Dir, "messages", message.ID+".json")
		read = func() error { _, err := store.message(message.ID); return err }
	case "watch":
		watch := Watch{Watcher: "cursor:" + newID(), Target: target.ID, ExpiresAt: time.Now().Add(time.Hour).Format(time.RFC3339Nano)}
		if err := store.saveWatch(watch); err != nil {
			t.Fatal(err)
		}
		path, err = store.watchPath(watch.Watcher, target.ID)
		read = func() error {
			due, err := store.consumeWatches(target.ID, time.Now())
			if due != nil {
				t.Fatal("unsupported watch returned due items", due)
			}
			return err
		}
	default:
		t.Fatal("unknown test family", family)
	}
	if err != nil {
		t.Fatal(err)
	}
	return path, read
}
