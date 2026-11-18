package adapters_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func adapterTestStore(t *testing.T) bus.Store {
	t.Helper()
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func registerAdapterSession(t *testing.T, store bus.Store, kind string) (model.Session, string) {
	t.Helper()
	var endpoint *model.Endpoint
	if kind == "claude" {
		endpoint = &model.Endpoint{Path: "/fixture/not-a-real-socket"}
	}
	session, key, err := store.Register(kind, model.NewUUID(), "/workspace", endpoint)
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

func adapterTestSession(t *testing.T, store bus.Store, id string) model.Session {
	t.Helper()
	var session model.Session
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
		DecodeState(agent.StateInput) (json.RawMessage, error)
	}); ok {
		if session.AdapterState, err = codec.DecodeState(agent.StateInput{State: session.AdapterState}); err != nil {
			t.Fatal(err)
		}
	}
	session.Receive = adapter.ReceiveCapabilities(session)
	return session
}

func adapterTestMessage(t *testing.T, store bus.Store, id string) model.Message {
	t.Helper()
	var message model.Message
	data, err := os.ReadFile(filepath.Join(store.Dir, "messages", id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatal(err)
	}
	return message
}

func saveAdapterTestSession(store bus.Store, session model.Session) error {
	return writeTestJSON(filepath.Join(store.Dir, "sessions", strings.ReplaceAll(session.ID, ":", "_")+".json"), session)
}
