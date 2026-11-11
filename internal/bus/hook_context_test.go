package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type contextHookAdapter struct {
	stubCodexAdapter
	handled    bool
	handlerErr error
}

func (a contextHookAdapter) Descriptor() agent.AdapterDescriptor {
	d := a.stubCodexAdapter.Descriptor()
	d.Kind, d.Label, d.ProcessNames = "context-hook", "Context Hook", nil
	return d
}

func (a contextHookAdapter) HandleHook(host agent.HookHost, _ agent.HookInput, id string, _ io.Writer) (bool, error) {
	if a.handlerErr != nil {
		return false, a.handlerErr
	}
	if a.handled {
		return true, host.UpdateSession(id, model.StateReady, false)
	}
	return false, nil
}

type hookContextFixture struct {
	service *Service
	subject model.Session
	input   []byte
}

func newHookContextFixture(t *testing.T, branch string, jobs int) hookContextFixture {
	t.Helper()
	registry, err := agent.NewRegistry(stubCodexAdapter{}, contextHookAdapter{handled: branch == "handled_approval"})
	if err != nil {
		t.Fatal(err)
	}
	store := Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: registry}
	service := &Service{Store: store}
	subject, _ := register(t, store, "context-hook")
	sender, key := register(t, store, "codex")
	if branch == "watch" {
		if err := store.Update(subject.ID, model.StateBusy, false); err != nil {
			t.Fatal(err)
		}
		if result, err := service.Watch(sender.ID, key, subject.ID); err != nil || result.Status != "watching" {
			t.Fatal("could not arm context fixture watch", err)
		}
	} else {
		if err := store.Update(subject.ID, model.StateApproval, false); err != nil {
			t.Fatal(err)
		}
		for range jobs {
			if msg, err := service.Send(t.Context(), SendRequest{Actor: sender.ID, Capability: key, To: subject.ID, MessageID: newID(), Body: "held context fixture"}); err != nil || !msg.Held {
				t.Fatal("context fixture was not held", err)
			}
		}
	}
	input, err := json.Marshal(agent.HookInput{SessionID: subject.NativeID, Event: "Stop"})
	if err != nil {
		t.Fatal(err)
	}
	return hookContextFixture{service: service, subject: subject, input: input}
}

func (f hookContextFixture) invoke(ctx context.Context) error {
	return f.service.Hook(ctx, f.subject.Kind, bytes.NewReader(f.input), &bytes.Buffer{})
}

func contextFixtureReceipt(ctx context.Context) model.Receipt {
	if err := ctx.Err(); err != nil {
		receipt := model.NewReceipt(model.StatusNotSent)
		receipt.Error = err.Error()
		return receipt.WithCause(err)
	}
	return model.NewReceipt(model.StatusSubmitted)
}

func assertHookContextOutcome(t *testing.T, fixture hookContextFixture, ids []string, want model.DeliveryStatus) {
	t.Helper()
	if len(ids) == 0 {
		t.Fatal("hook did not attempt delivery")
	}
	for _, id := range ids {
		msg, err := fixture.service.Store.message(id)
		if err != nil || msg.Status != want || msg.Receipt == nil || msg.Receipt.Status != want || msg.Held || msg.AcknowledgedAt != "" {
			t.Fatalf("durable context outcome=%+v error=%v, want %s with receipt and no hold/ACK", msg, err, want)
		}
		if want == model.StatusNotSent && msg.Receipt.Error != context.Canceled.Error() {
			t.Fatalf("cancellation receipt=%+v", msg.Receipt)
		}
	}
	assertHookContextSession(t, fixture)
}

func assertHookContextSession(t *testing.T, fixture hookContextFixture) {
	t.Helper()
	current, err := fixture.service.Store.session(fixture.subject.ID)
	if err != nil || current.State != model.StateReady || current.ApprovalHeld != 0 || current.KeyHash != fixture.subject.KeyHash || current.Generation != fixture.subject.Generation {
		t.Fatal("context handling changed credentials/generation or left lifecycle/approval state incomplete", err)
	}
}

type hookContextKey struct{}

// The old API supplied Background. Each existing delivery branch now inherits
// cancellation and values without changing the durable outcome contract.
func TestHookDeliveryInheritsCanceledParentBeforeDelivery(t *testing.T) {
	for _, branch := range []string{"watch", "handled_approval", "ordinary_approval"} {
		t.Run(branch, func(t *testing.T) {
			fixture := newHookContextFixture(t, branch, 1)
			parent, cancel := context.WithDeadline(context.WithValue(t.Context(), hookContextKey{}, "caller"), time.Now().Add(time.Minute))
			cancel()
			var seen context.Context
			var ids []string
			fixture.service.Deliver = func(ctx context.Context, msg model.Message, _ model.Session) model.Receipt {
				seen = ctx
				ids = append(ids, msg.ID)
				return contextFixtureReceipt(ctx)
			}
			if err := fixture.invoke(parent); err != nil {
				t.Fatal(err)
			}
			if seen == nil || !errors.Is(seen.Err(), context.Canceled) || seen.Value(hookContextKey{}) != "caller" {
				t.Fatal("hook lost caller cancellation/value")
			}
			want, _ := parent.Deadline()
			if got, ok := seen.Deadline(); !ok || !got.Equal(want) {
				t.Fatal("hook lost caller deadline")
			}
			assertHookContextOutcome(t, fixture, ids, model.StatusNotSent)
		})
	}
}

func startControlledHook(t *testing.T, fixture hookContextFixture, parent context.Context, release chan struct{}) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	go func() {
		done <- fixture.invoke(parent)
		close(done)
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("controlled hook did not stop during cleanup")
		}
	})
	return done
}

func awaitHookContext(t *testing.T, ready <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-ready:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("hook transport did not start")
		return nil
	}
}

func awaitHookDone(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("hook did not finish")
	}
}

// Canceling the caller must release a waiting transport without the fixture
// releasing its gate, and the resulting receipt must finish durably.
func TestHookDeliveryInheritsParentCancellationDuringDelivery(t *testing.T) {
	for _, branch := range []string{"watch", "handled_approval", "ordinary_approval"} {
		t.Run(branch, func(t *testing.T) {
			fixture := newHookContextFixture(t, branch, 1)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ready, release := make(chan context.Context, 1), make(chan struct{})
			var ids []string
			fixture.service.Deliver = func(ctx context.Context, msg model.Message, _ model.Session) model.Receipt {
				ids = append(ids, msg.ID)
				ready <- ctx
				select {
				case <-ctx.Done():
				case <-release:
				}
				return contextFixtureReceipt(ctx)
			}
			done := startControlledHook(t, fixture, parent, release)
			seen := awaitHookContext(t, ready)
			cancel()
			if seen.Done() == nil || !errors.Is(seen.Err(), context.Canceled) {
				t.Fatal("transport did not inherit parent cancellation")
			}
			awaitHookDone(t, done)
			assertHookContextOutcome(t, fixture, ids, model.StatusNotSent)
		})
	}
}

func TestHookDeliveryBackgroundControl(t *testing.T) {
	for _, branch := range []string{"watch", "handled_approval", "ordinary_approval"} {
		t.Run(branch, func(t *testing.T) {
			fixture := newHookContextFixture(t, branch, 1)
			var ids []string
			fixture.service.Deliver = func(ctx context.Context, msg model.Message, _ model.Session) model.Receipt {
				ids = append(ids, msg.ID)
				if ctx.Err() != nil || ctx.Done() != nil {
					t.Error("Background control was canceled")
				}
				return contextFixtureReceipt(ctx)
			}
			if err := fixture.invoke(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertHookContextOutcome(t, fixture, ids, model.StatusSubmitted)
		})
	}
}

func TestHookContextCancellationSettlesEveryReleasedJob(t *testing.T) {
	for _, branch := range []string{"handled_approval", "ordinary_approval"} {
		t.Run(branch, func(t *testing.T) {
			fixture := newHookContextFixture(t, branch, 2)
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			ready, release := make(chan context.Context, 1), make(chan struct{})
			var ids []string
			fixture.service.Deliver = func(ctx context.Context, msg model.Message, _ model.Session) model.Receipt {
				ids = append(ids, msg.ID)
				if len(ids) == 1 {
					ready <- ctx
					select {
					case <-ctx.Done():
					case <-release:
					}
				}
				return contextFixtureReceipt(ctx)
			}
			done := startControlledHook(t, fixture, parent, release)
			seen := awaitHookContext(t, ready)
			cancel()
			if seen.Done() == nil || !errors.Is(seen.Err(), context.Canceled) {
				t.Fatal("first released job lost the caller context")
			}
			awaitHookDone(t, done)
			if len(ids) != 2 {
				t.Fatalf("attempted %d jobs, want both resolved jobs settled", len(ids))
			}
			assertHookContextOutcome(t, fixture, ids, model.StatusNotSent)
		})
	}
}

func TestHookContextCancellationKeepsUncertainReceipt(t *testing.T) {
	fixture := newHookContextFixture(t, "watch", 1)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ready, release := make(chan context.Context, 1), make(chan struct{})
	var ids []string
	fixture.service.Deliver = func(ctx context.Context, msg model.Message, _ model.Session) model.Receipt {
		ids = append(ids, msg.ID)
		// The fixture's native acceptance boundary precedes this signal.
		ready <- ctx
		select {
		case <-ctx.Done():
		case <-release:
		}
		receipt := model.NewReceipt(model.StatusUnknownWrite)
		if err := ctx.Err(); err != nil {
			receipt.Error = err.Error()
			return receipt.WithCause(err)
		}
		return receipt
	}
	done := startControlledHook(t, fixture, parent, release)
	seen := awaitHookContext(t, ready)
	cancel()
	if !errors.Is(seen.Err(), context.Canceled) {
		t.Fatal("accepted submission did not receive cancellation")
	}
	awaitHookDone(t, done)
	assertHookContextOutcome(t, fixture, ids, model.StatusUnknownWrite)
}

func TestHookContextCancellationKeepsDecodeError(t *testing.T) {
	fixture := newHookContextFixture(t, "watch", 1)
	fixture.input = []byte("{")
	before := journalSnapshot(t, fixture.service.Store)
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fixture.invoke(parent); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("first error=%v, want malformed JSON before cancellation", err)
	}
	assertForwardingChanges(t, fixture.service.Store, before)
}

func TestHookContextCancellationKeepsHandlerError(t *testing.T) {
	fixture := newHookContextFixture(t, "ordinary_approval", 1)
	handlerErr := errors.New("handler fails before native delivery")
	registry, err := agent.NewRegistry(stubCodexAdapter{}, contextHookAdapter{handlerErr: handlerErr})
	if err != nil {
		t.Fatal(err)
	}
	fixture.service.Store.Registry = registry
	// Owner/build refresh legitimately runs before the handler; preseed it so
	// the snapshot isolates handler failure from that existing earlier stage.
	if err := fixture.service.Store.rememberOwner(fixture.subject.ID, fixture.subject.Kind, false); err != nil {
		t.Fatal(err)
	}
	before := journalSnapshot(t, fixture.service.Store)
	fixture.service.Deliver = func(context.Context, model.Message, model.Session) model.Receipt {
		t.Error("failed handler reached transport")
		return model.NewReceipt(model.StatusSubmitted)
	}
	parent, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fixture.invoke(parent); !errors.Is(err, handlerErr) {
		t.Fatalf("first error=%v, want handler failure", err)
	}
	assertForwardingChanges(t, fixture.service.Store, before)
}
