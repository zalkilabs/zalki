package adapters_test

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters/cursor"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// replacementSetup makes the two possible setup orders observable without
// sleeps. The completion wait releases real Confirm first. Without that wait,
// replacement releases it instead, and doctor observes the failed old setup.
type replacementSetup struct {
	ctx                                                  context.Context
	waiting, replaced, allow, confirmed, exited, checked chan struct{}
}

func newReplacementSetup(t *testing.T) *replacementSetup {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	s := &replacementSetup{ctx: ctx, waiting: make(chan struct{}), replaced: make(chan struct{}),
		allow: make(chan struct{}), confirmed: make(chan struct{}), exited: make(chan struct{}), checked: make(chan struct{})}
	done := make(chan struct{})
	go func() { defer close(done); s.coordinate() }()
	t.Cleanup(func() { cancel(); <-done })
	return s
}

func (s *replacementSetup) coordinate() {
	defer close(s.checked)
	waiting := false
	select {
	case <-s.waiting:
		waiting = true
	case <-s.replaced:
	case <-s.ctx.Done():
	}
	close(s.allow)
	if waiting {
		select {
		case <-s.replaced:
		case <-s.ctx.Done():
		}
	} else {
		select {
		case <-s.exited:
		case <-s.ctx.Done():
		}
	}
}

func (s *replacementSetup) waitConfirmed(t *testing.T) {
	t.Helper()
	close(s.waiting)
	select {
	case <-s.confirmed:
	case <-s.exited:
		t.Fatal("old listener exited before confirmation completed")
	case <-s.ctx.Done():
		t.Fatal("old listener confirmation did not complete", s.ctx.Err())
	}
}

func (s *replacementSetup) replacementFinished(t *testing.T) {
	t.Helper()
	// The preceding Register and duplicate-listener check have both completed.
	// In the omission control, let old Confirm fail and Close finish before
	// doctor reads health; this preserves the original two failure messages.
	close(s.replaced)
	select {
	case <-s.checked:
	case <-s.ctx.Done():
		t.Fatal("replacement setup ordering did not complete", s.ctx.Err())
	}
}

type replacementLease struct {
	cursor.ListenerHandle
	setup *replacementSetup
}

func (l *replacementLease) Confirm(policy func(model.SessionView) (json.RawMessage, error)) error {
	select {
	case <-l.setup.allow:
	case <-l.setup.ctx.Done():
		return l.setup.ctx.Err()
	}
	if err := l.ListenerHandle.Confirm(policy); err != nil {
		return err
	}
	close(l.setup.confirmed)
	return nil
}

func (l *replacementLease) Close(cause error) {
	l.ListenerHandle.Close(cause)
	close(l.setup.exited)
}

func startReplacementListener(t *testing.T, store bus.Store, actor, key string) (func(), *wakeOutput, *replacementSetup) {
	t.Helper()
	setup := newReplacementSetup(t)
	stop, output := startRecoveryListenerRun(t, func(ctx context.Context, output io.Writer) error {
		acquire := func(actor, key string) (cursor.ListenerHandle, error) {
			lease, err := store.AcquireListenerLease(actor, key)
			if err != nil {
				return nil, err
			}
			return &replacementLease{ListenerHandle: lease, setup: setup}, nil
		}
		return cursor.Listen(ctx, acquire, agent.InspectEndpoint, actor, key, output)
	})
	return stop, output, setup
}
