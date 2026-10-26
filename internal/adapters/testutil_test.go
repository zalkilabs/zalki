package adapters_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

// newTestID returns a random UUID-shaped identifier, as native client thread ids are.
func newTestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6], b[8] = b[6]&15|64, b[8]&63|128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func adapterTestStore(t *testing.T) bus.Store {
	t.Helper()
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func registerAdapterSession(t *testing.T, store bus.Store, kind string) (bus.Session, string) {
	t.Helper()
	var endpoint *bus.Endpoint
	if kind == "claude" {
		endpoint = &bus.Endpoint{Path: "/fixture/not-a-real-socket"}
	}
	session, key, err := store.Register(kind, newTestID(), "/workspace", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return session, key
}

func writeTestJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

func adapterTestSession(t *testing.T, store bus.Store, id string) bus.Session {
	t.Helper()
	var session bus.Session
	data, err := os.ReadFile(filepath.Join(store.Dir, "sessions", strings.ReplaceAll(id, ":", "_")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &session); err != nil {
		t.Fatal(err)
	}
	adapter, err := store.Registry.Lookup(session.Kind)
	if err != nil {
		t.Fatal(err)
	}
	if codec, ok := adapter.(interface {
		DecodeState(*bus.Session, json.RawMessage) error
	}); ok {
		if err := codec.DecodeState(&session, data); err != nil {
			t.Fatal(err)
		}
	}
	session.Receive = adapter.ReceiveCapabilities(session)
	return session
}

func adapterTestMessage(t *testing.T, store bus.Store, id string) bus.Message {
	t.Helper()
	var message bus.Message
	data, err := os.ReadFile(filepath.Join(store.Dir, "messages", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func saveAdapterTestSession(store bus.Store, session bus.Session) error {
	return writeTestJSON(filepath.Join(store.Dir, "sessions", strings.ReplaceAll(session.ID, ":", "_")+".json"), session)
}

func adapterTestCursorState(t *testing.T, session bus.Session) bus.CursorListenerState {
	t.Helper()
	var state bus.CursorListenerState
	if err := json.Unmarshal(session.AdapterState, &state); err != nil {
		t.Fatal(err)
	}
	return state
}
