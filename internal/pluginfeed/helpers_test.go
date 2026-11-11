package pluginfeed

import (
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func fixtureStore(t *testing.T) bus.Store {
	t.Helper()
	return bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: testRegistry()}
}

func register(t *testing.T, store bus.Store, kind string) (model.Session, string) {
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
