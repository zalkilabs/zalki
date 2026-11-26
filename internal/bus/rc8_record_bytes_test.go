package bus

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/statefile"
)

// The files in testdata/rc8 were written by the rc.8 release. An update must
// read them and must write the same bytes back. These tests pin that for the
// record IO under internal/statefile: if the record encoding or the strict
// decoding moves to another package, they must pass without any change to the
// expected bytes.

// rc8Record reads one rc.8 record with the production read path and encodes it
// again with the production write path.
type rc8Record struct {
	file    string
	reencod func(t *testing.T, store Store, path string) []byte
}

func rc8Records() []rc8Record {
	return []rc8Record{
		{"sessions/claude_00000000-0000-4000-8000-0000000000d4.json", reencodeSession},
		{"messages/2dc625a2-d7b2-4874-a128-f05ea54a5422.json", reencodeMessage},
		{"messages/50d11489-eb0d-47e3-8fd2-8e58874c2230.json", reencodeMessage},
		{"messages/56930e71-e286-438c-a542-4932b5db225f.json", reencodeMessage},
		{"messages/81ac70cf-e482-40ed-ba84-b73af96b43a9.json", reencodeMessage},
		{"messages/bcaeec6d-8ca5-4e14-925d-c5f2bc6486df.json", reencodeMessage},
		{"messages/d8624c29-cd85-8453-a146-42784d28c66d.json", reencodeMessage},
		{"flood/claude_00000000-0000-4000-8000-0000000000d4.json", reencodeFlood},
		{"watches/cursor_00000000-0000-4000-8000-0000000000a1.json", reencodeWatch},
		{"events/meta.json", reencodeEventMeta},
		{"events/00000000000000000001.json", reencodeEvent},
		{"events/00000000000000000005.json", reencodeEvent},
		{"events/00000000000000000016.json", reencodeEvent},
	}
}

func rc8Path(file string) string { return filepath.Join("testdata", "rc8", filepath.FromSlash(file)) }

func readRC8(t *testing.T, file string) []byte {
	t.Helper()
	raw, err := os.ReadFile(rc8Path(file))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func reencodeSession(t *testing.T, store Store, path string) []byte {
	t.Helper()
	session, err := store.readSession(path)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	return mustEncodeState(t, session)
}

func reencodeMessage(t *testing.T, _ Store, path string) []byte {
	t.Helper()
	var message model.Message
	if err := readJSON(path, &message); err != nil {
		t.Fatalf("read message: %v", err)
	}
	return mustEncodeState(t, message)
}

func reencodeFlood(t *testing.T, _ Store, path string) []byte {
	t.Helper()
	var record floodRecord
	if err := readJSON(path, &record); err != nil {
		t.Fatalf("read flood record: %v", err)
	}
	return mustEncodeState(t, record)
}

func reencodeWatch(t *testing.T, _ Store, path string) []byte {
	t.Helper()
	var watch Watch
	if err := readJSON(path, &watch); err != nil {
		t.Fatalf("read watch: %v", err)
	}
	return mustEncodeState(t, watch)
}

func reencodeEventMeta(t *testing.T, _ Store, path string) []byte {
	t.Helper()
	raw, err := statefile.ReadBytes(path, maxStateFileBytes)
	if err != nil {
		t.Fatal(err)
	}
	var metadata eventMetadata
	if err := decodeEventJSON(raw, &metadata); err != nil {
		t.Fatalf("decode event metadata: %v", err)
	}
	return mustEncodeState(t, metadata)
}

func reencodeEvent(t *testing.T, _ Store, path string) []byte {
	t.Helper()
	raw, err := statefile.ReadBytes(path, maxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	var record eventRecord
	if err := decodeEventJSON(raw, &record); err != nil {
		t.Fatalf("decode event record: %v", err)
	}
	if err := record.validate(); err != nil {
		t.Fatalf("event record is not valid: %v", err)
	}
	encoded, err := statefile.EncodeJSON(record, maxEventBytes)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func mustEncodeState(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := encodeStateJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestRC8RecordsReencodeToTheSameBytes(t *testing.T) {
	store := fixtureStore(t)
	for _, record := range rc8Records() {
		t.Run(record.file, func(t *testing.T) {
			want := readRC8(t, record.file)
			got := record.reencod(t, store, rc8Path(record.file))
			if !bytes.Equal(got, want) {
				t.Fatalf("re-encoded record differs from the rc.8 bytes\ngot:  %q\nwant: %q", got, want)
			}
		})
	}
}

// The store writers publish the rc.8 bytes unchanged: same encoding, same
// permissions.
func TestRC8RecordsAreWrittenWithTheSameBytes(t *testing.T) {
	store := fixtureStore(t)
	if err := store.locked(func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	t.Run("flood record", func(t *testing.T) {
		const file = "flood/claude_00000000-0000-4000-8000-0000000000d4.json"
		var record floodRecord
		if err := readJSON(rc8Path(file), &record); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(store.dir, filepath.FromSlash(file))
		if err := store.locked(func() error { return store.saveFloodState(path, record) }); err != nil {
			t.Fatal(err)
		}
		requireSameFile(t, path, readRC8(t, file))
	})
	t.Run("watch", func(t *testing.T) {
		const file = "watches/cursor_00000000-0000-4000-8000-0000000000a1.json"
		var watch Watch
		if err := readJSON(rc8Path(file), &watch); err != nil {
			t.Fatal(err)
		}
		path, err := store.watchPath(watch.Watcher, watch.Target)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.locked(func() error { return store.saveWatch(watch) }); err != nil {
			t.Fatal(err)
		}
		requireSameFile(t, path, readRC8(t, file))
	})
	for _, file := range []string{
		"messages/2dc625a2-d7b2-4874-a128-f05ea54a5422.json",
		"messages/bcaeec6d-8ca5-4e14-925d-c5f2bc6486df.json",
	} {
		t.Run(file, func(t *testing.T) {
			var message model.Message
			if err := readJSON(rc8Path(file), &message); err != nil {
				t.Fatal(err)
			}
			if err := store.locked(func() error { return store.saveMessage(message) }); err != nil {
				t.Fatal(err)
			}
			requireSameFile(t, filepath.Join(store.dir, "messages", message.ID+".json"), readRC8(t, file))
		})
	}
}

func requireSameFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("written file differs from the rc.8 bytes\ngot:  %q\nwant: %q", got, want)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v, want 0600", info.Mode().Perm())
	}
}
