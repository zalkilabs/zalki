package pluginfeed

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func fixtureStore(t *testing.T) bus.Store {
	t.Helper()
	return bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: testRegistry(), PluginDelivery: CheckPluginDelivery}
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

// nextFeed selects the next feed event for a launch that starts now.
func nextFeed(f PluginFeed) (PluginDelivery, error) {
	status, _ := f.PluginStatus() // a load error comes back from NextPluginFeed too
	return f.NextPluginFeed(&status)
}

// testExecutable is an absolute, pinned-able executable for plugin records
// whose child never runs in the test: the test binary itself.
func testExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return path
}
