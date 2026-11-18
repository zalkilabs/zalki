package plugins

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

const accessStderrSentinel = "access-private-stderr-sentinel"

// No capability is required by this hello: extended-only grants must reach the
// ordinary child failure, not a host-made missing-feed hint that hides stderr.
func failAfterAccessStderr(state string) error {
	if err := recordHostFrames(state); err != nil {
		return err
	}
	if _, err := os.Stderr.WriteString(accessStderrSentinel); err != nil {
		return err
	}
	return errors.New("fixture stopped after stderr")
}

func TestHostStderrUsesMessageAccess(t *testing.T) {
	for _, test := range []struct {
		name   string
		access pluginfeed.PluginAccess
		hide   bool
	}{
		{"receive", pluginfeed.PluginAccess{Receive: true}, true},
		{"send-only", pluginfeed.PluginAccess{Send: []string{"session"}}, true},
		{"read-only", pluginfeed.PluginAccess{Read: []string{"session"}}, true},
		{"status-only", pluginfeed.PluginAccess{Status: true}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkHostStderrAccess(t, test.access, test.hide)
		})
	}
}

func stderrAccessStore(t *testing.T, access pluginfeed.PluginAccess) bus.Store {
	t.Helper()
	store, session := hostStore(t)
	if len(access.Send) > 0 {
		access.Send = []string{session.ID}
	}
	if len(access.Read) > 0 {
		access.Read = []string{session.ID}
	}
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	if err := feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(access); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	return store
}

func checkHostStderrAccess(t *testing.T, access pluginfeed.PluginAccess, hide bool) {
	t.Helper()
	store := stderrAccessStore(t, access)
	if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte("access-stderr"), 0600); err != nil {
		t.Fatal(err)
	}
	err := Run(t.Context(), store, "fake-notifier", io.Discard, nil)
	if frames := readHostFrames(t, store); !strings.Contains(frames, `"hello_ok"`) {
		t.Fatalf("STDERR_ACCESS_DID_NOT_COMPLETE_HELLO: %q", frames)
	}
	if err == nil {
		t.Fatal("STDERR_ACCESS_CHILD_DID_NOT_FAIL")
	}
	if hide {
		if err.Error() != "plugin request host failed" {
			t.Fatalf("MESSAGE_ACCESS_STDERR_VISIBLE: %v", err)
		}
	} else if !strings.Contains(err.Error(), accessStderrSentinel) {
		t.Fatalf("STATUS_ONLY_STDERR_WITHHELD: %v", err)
	}
}
