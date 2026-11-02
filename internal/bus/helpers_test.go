package bus

import (
	"path/filepath"
	"testing"
)

func fixtureStore(t *testing.T) Store {
	t.Helper()
	return Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: testRegistry()}
}

func register(t *testing.T, store Store, kind string) (Session, string) {
	t.Helper()
	var endpoint *Endpoint
	if kind == "claude" {
		endpoint = &Endpoint{Path: "/fixture/not-a-real-socket"}
	}
	session, key, err := store.Register(kind, newID(), "/workspace", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return session, key
}

// newTestService gives each test a private journal and the core stub registry.
// Tests set Deliver explicitly when they need a transport stub.
func newTestService(t *testing.T) Service {
	t.Helper()
	return Service{Store: fixtureStore(t)}
}

func registerPair(t *testing.T, store Store, senderKind, targetKind string) (Session, string, Session, string) {
	t.Helper()
	sender, senderKey := register(t, store, senderKind)
	target, targetKey := register(t, store, targetKind)
	return sender, senderKey, target, targetKey
}
