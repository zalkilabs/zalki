package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// These tests characterize consumption, including partial deletion on error.
// They do not endorse the behavior or extend Watch's public contract.
func watchConsumptionFixture(t *testing.T) (Store, string, string) {
	t.Helper()
	store := fixtureStore(t)
	target := "codex:" + newID()
	dir := filepath.Join(store.Dir, "watches", strings.ReplaceAll(target, ":", "_"))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return store, target, dir
}

func writeConsumptionWatch(t *testing.T, dir, name string, watch Watch) []byte {
	t.Helper()
	raw, err := json.Marshal(watch)
	if err != nil {
		t.Fatal(err)
	}
	writeConsumptionBytes(t, filepath.Join(dir, name), raw)
	return raw
}

func writeConsumptionBytes(t *testing.T, path string, raw []byte) {
	t.Helper()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func assertConsumptionFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if want == nil {
		if !os.IsNotExist(err) {
			t.Fatal("consumed watch remains", path, err)
		}
		return
	}
	if err != nil || string(got) != string(want) {
		t.Fatal("retained watch bytes changed", path, err)
	}
}

func TestWatchConsumptionRecordDisposition(t *testing.T) {
	asOf := time.Now().UTC()
	at := func(delta time.Duration) string { return asOf.Add(delta).Format(time.RFC3339Nano) }
	for _, test := range []struct {
		name, created, expires string
		wrongTarget, due, kept bool
	}{
		{"missing creation", "", at(time.Hour), false, true, false},
		{"malformed creation", "bad", at(time.Hour), false, true, false},
		{"creation before cutoff", at(-time.Minute), at(time.Hour), false, true, false},
		{"creation at cutoff", at(0), at(time.Hour), false, true, false},
		{"creation after cutoff", at(time.Minute), at(time.Hour), false, false, true},
		{"expired", "", at(-time.Hour), false, false, false},
		{"malformed expiry", "", "bad", false, false, false},
		{"wrong target before new creation", at(time.Minute), at(time.Hour), true, false, false},
		{"expired before new creation", at(time.Minute), at(-time.Hour), false, false, false},
		{"malformed expiry before new creation", at(time.Minute), "bad", false, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, target, dir := watchConsumptionFixture(t)
			watch := Watch{Schema: recordSchema, Watcher: "cursor:" + newID(), Target: target, CreatedAt: test.created, ExpiresAt: test.expires}
			if test.wrongTarget {
				watch.Target = "claude:" + newID()
			}
			raw := writeConsumptionWatch(t, dir, "entry", watch)
			due, err := store.consumeWatches(target, asOf)
			var want []Watch
			if test.due {
				want = []Watch{watch}
			}
			if err != nil || !reflect.DeepEqual(due, want) {
				t.Fatalf("due=%+v error=%v, want %+v", due, err, want)
			}
			if !test.kept {
				raw = nil
			}
			assertConsumptionFile(t, filepath.Join(dir, "entry"), raw)
		})
	}
}

func TestWatchConsumptionUsesFileOrderAndSkipsDirectories(t *testing.T) {
	store, target, dir := watchConsumptionFixture(t)
	now := time.Now().UTC()
	first := Watch{Schema: recordSchema, Watcher: "cursor:ffffffff-ffff-4fff-8fff-ffffffffffff", Target: target, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano)}
	second := first
	second.Watcher = "cursor:00000000-0000-4000-8000-000000000000"
	// Create in reverse order, and use a non-JSON filename for the first record.
	writeConsumptionWatch(t, dir, "02.json", second)
	writeConsumptionWatch(t, dir, "00.entry", first)
	nested := filepath.Join(dir, "01.directory", "bad.json")
	if err := os.Mkdir(filepath.Dir(nested), 0700); err != nil {
		t.Fatal(err)
	}
	writeConsumptionBytes(t, nested, []byte("{bad"))
	due, err := store.consumeWatches(target, now)
	if err != nil || !reflect.DeepEqual(due, []Watch{first, second}) {
		t.Fatal("consumption lost lexical file order", due, err)
	}
	assertConsumptionFile(t, nested, []byte("{bad"))
	assertConsumptionFile(t, filepath.Join(dir, "00.entry"), nil)
	assertConsumptionFile(t, filepath.Join(dir, "02.json"), nil)
}

func TestWatchConsumptionReadErrorDiscardsDueAfterPartialDeletion(t *testing.T) {
	for _, raw := range []string{"{bad", `{"schema":99,"expires_at":"bad","target":"wrong"}`} {
		t.Run(raw, func(t *testing.T) { checkWatchConsumptionReadError(t, raw) })
	}
}

func checkWatchConsumptionReadError(t *testing.T, raw string) {
	t.Helper()
	store, target, dir := watchConsumptionFixture(t)
	now := time.Now().UTC()
	watch := Watch{Schema: recordSchema, Watcher: "cursor:" + newID(), Target: target, ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano)}
	later := writeConsumptionWatch(t, dir, "03.later", watch)
	writeConsumptionBytes(t, filepath.Join(dir, "02.bad"), []byte(raw))
	expired := watch
	expired.ExpiresAt = now.Add(-time.Hour).Format(time.RFC3339Nano)
	writeConsumptionWatch(t, dir, "01.expired", expired)
	writeConsumptionWatch(t, dir, "00.due", watch)
	due, err := store.consumeWatches(target, now)
	if due != nil || err == nil {
		t.Fatal("read failure retained a partial due slice or lost its error", due, err)
	}
	assertWatchConsumptionReadError(t, raw, err)
	assertConsumptionFile(t, filepath.Join(dir, "00.due"), nil)
	assertConsumptionFile(t, filepath.Join(dir, "01.expired"), nil)
	assertConsumptionFile(t, filepath.Join(dir, "02.bad"), []byte(raw))
	assertConsumptionFile(t, filepath.Join(dir, "03.later"), later)
}

func assertWatchConsumptionReadError(t *testing.T, raw string, err error) {
	t.Helper()
	if raw == "{bad" {
		var syntax *json.SyntaxError
		if !errors.As(err, &syntax) {
			t.Fatal("JSON syntax error changed", err)
		}
		return
	}
	if model.ErrorCodeOf(err) != model.CodeUnsupportedSchema || err.Error() != "unsupported_schema: unsupported journal schema 99" {
		t.Fatal("schema rejection no longer precedes invalid expiry/target cleanup", err)
	}
}

func TestWatchConsumptionSetupResults(t *testing.T) {
	for _, setup := range []string{"invalid target", "missing directory", "empty directory", "not a directory"} {
		t.Run(setup, func(t *testing.T) { checkWatchConsumptionSetup(t, setup) })
	}
}

func checkWatchConsumptionSetup(t *testing.T, setup string) {
	t.Helper()
	store, target, dir := watchConsumptionFixture(t)
	if setup != "empty directory" {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
	}
	if setup == "invalid target" {
		target = "bad"
	}
	if setup == "not a directory" {
		writeConsumptionBytes(t, dir, []byte("unchanged"))
	}
	due, err := store.consumeWatches(target, time.Now())
	if due != nil {
		t.Fatal("setup result returned a non-nil due slice", due)
	}
	switch setup {
	case "invalid target":
		if model.ErrorCodeOf(err) != model.CodeInvalidIdentity {
			t.Fatal("invalid target was not rejected before directory handling", err)
		}
	case "not a directory":
		var pathErr *os.PathError
		if !errors.As(err, &pathErr) || pathErr.Path != dir {
			t.Fatal("directory read error changed", err)
		}
		assertConsumptionFile(t, dir, []byte("unchanged"))
	default:
		if err != nil {
			t.Fatal("empty/missing directory failed", err)
		}
	}
}
