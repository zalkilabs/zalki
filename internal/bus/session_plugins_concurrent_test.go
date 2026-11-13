package bus

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type gatedPluginCursor struct {
	stubCursorAdapter
	armed   *atomic.Bool
	entered chan struct{}
	resume  chan struct{}
}

func (a gatedPluginCursor) ReceiveCapabilities(v model.Session) model.ReceiveCapabilities {
	if v.Generation != "" && a.armed.CompareAndSwap(true, false) {
		close(a.entered)
		<-a.resume
	}
	return a.stubCursorAdapter.ReceiveCapabilities(v)
}

func TestSessionPluginReplacementCarriesConcurrentMute(t *testing.T) {
	var armed atomic.Bool
	adapter := gatedPluginCursor{armed: &armed, entered: make(chan struct{}), resume: make(chan struct{})}
	s := fixtureStore(t)
	var err error
	s.Registry, err = agent.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	current, key := register(t, s, "cursor")
	armed.Store(true)
	done := make(chan error, 1)
	go func() { _, _, err := s.Register("cursor", current.NativeID, current.CWD, nil); done <- err }()
	select {
	case <-adapter.entered:
	case <-time.After(10 * time.Second):
		close(adapter.resume)
		t.Fatal("Z6_CONCURRENT_REGISTRATION_BARRIER_REACHED")
	}
	if err := SetSessionPlugin(s, current.ID, key, "external", true); err != nil {
		close(adapter.resume)
		t.Fatal(err)
	}
	_, muted := pluginPolicyForTest(t, s, current.ID)
	close(adapter.resume)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, p := pluginPolicyForTest(t, s, current.ID)
	if after.Generation == current.Generation || p.Muted["external"].Actor != current.ID || p.LastChange.ID != muted.LastChange.ID {
		t.Fatal("Z6_REPLACEMENT_CARRIES_CONCURRENT_OVERLAY_AND_AUDIT")
	}
}
