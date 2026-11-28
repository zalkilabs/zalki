package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func portCommand() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	var out, errOut bytes.Buffer
	cmd.SetContext(context.Background())
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	return cmd, &out, &errOut
}

// The fake keeps a real store for the runtime lease, whose fields are private.
type fakeSession struct {
	request    bus.SendRequest
	sendErr    error
	store      bus.Store
	tracked    []string
	trackErr   error
	registry   *agent.Registry
	acquired   []string
	acquireErr error
}

func (f *fakeSession) Send(_ context.Context, request bus.SendRequest) (model.Message, error) {
	f.request = request
	return model.Message{ID: "m1", From: request.Actor, To: request.To}, f.sendErr
}

func (f *fakeSession) TrackRuntime(role, actor string) (*bus.RuntimeLease, error) {
	f.tracked = append(f.tracked, role, actor)
	if f.trackErr != nil {
		return nil, f.trackErr
	}
	return f.store.TrackRuntime(role, actor)
}

func (f *fakeSession) AcquireListenerLease(actor, key string) (*bus.Lease, error) {
	f.acquired = append(f.acquired, actor, key)
	return nil, f.acquireErr
}

func (f *fakeSession) Registry() *agent.Registry { return f.registry }

func TestRunSendMapsTheRequestAndUsesTheEnvironmentCapability(t *testing.T) {
	fake := &fakeSession{}
	cmd, out, _ := portCommand()
	args := sendArgs{Actor: "claude:a", To: "codex:b", MessageID: "id-1", Body: "hello"}
	if err := runSend(cmd, fake, args, "secret"); err != nil {
		t.Fatal(err)
	}
	want := bus.SendRequest{Actor: "claude:a", Capability: "secret", To: "codex:b", MessageID: "id-1", Body: "hello"}
	if fake.request != want {
		t.Fatalf("request = %+v, want %+v", fake.request, want)
	}
	if strings.Contains(out.String(), "secret") || !strings.Contains(out.String(), "m1") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestRunSendReturnsTheSendError(t *testing.T) {
	fake := &fakeSession{sendErr: errors.New("not sent")}
	cmd, out, _ := portCommand()
	if err := runSend(cmd, fake, sendArgs{}, "k"); !errors.Is(err, fake.sendErr) {
		t.Fatalf("error = %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("output = %q, want none on failure", out.String())
	}
}

type listenerAdapter struct {
	actor, key string
	handle     agent.ListenerHandle
	err        error
	during     func()
}

func (*listenerAdapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "cursor", Label: "Cursor fixture", ProcessNames: []string{"cursor"}, OwnerRole: "client", Executable: "cursor"}
}

func (*listenerAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{IdleWake: true, Delivery: "listener_fixture", Inbox: model.InboxNative, BusyDelivery: model.DeliveryBetweenTools}
}

func (*listenerAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusNotSent)
}

func (a *listenerAdapter) Listen(_ context.Context, acquire func(actor, key string) (agent.ListenerHandle, error), actor, key string, _ io.Writer) error {
	a.actor, a.key = actor, key
	if a.during != nil {
		a.during()
	}
	handle, err := acquire(actor, key)
	a.handle = handle
	if err != nil {
		return err
	}
	return a.err
}

func runtimeRecords(t *testing.T, state string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(state, "runtimes"))
	if errors.Is(err, os.ErrNotExist) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(entries)
}

func TestRunCursorListenKeepsTheRuntimeRecordWhileListening(t *testing.T) {
	const actor = "cursor:00000000-0000-4000-8000-000000000000"
	state := filepath.Join(t.TempDir(), "state")
	adapter := &listenerAdapter{err: errors.New("stopped")}
	registry, err := agent.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeSession{registry: registry, store: bus.StoreAt(state, bus.WithRegistry(registry)), acquireErr: errors.New("no lease")}
	var during int
	adapter.during = func() { during = runtimeRecords(t, state) }
	cmd, _, _ := portCommand()
	// The fake lease source fails, so Listen returns that error, not the adapter's.
	if err := runCursorListen(cmd, fake, actor, "key"); !errors.Is(err, fake.acquireErr) {
		t.Fatalf("error = %v, want the lease error", err)
	}
	if got := strings.Join(fake.tracked, ","); got != "cursor_listener,"+actor {
		t.Fatalf("tracked = %q", got)
	}
	if got := strings.Join(fake.acquired, ","); got != actor+",key" || adapter.actor != actor || adapter.key != "key" {
		t.Fatalf("acquired %q, listen actor %q key %q", got, adapter.actor, adapter.key)
	}
	if adapter.handle != nil {
		t.Fatalf("a failed lease reached the adapter as %#v, want a nil handle", adapter.handle)
	}
	if during != 1 || runtimeRecords(t, state) != 0 {
		t.Fatalf("runtime records: %d while listening, %d after; want 1 and 0", during, runtimeRecords(t, state))
	}
}

func TestRunCursorListenStopsWhenTheRuntimeCannotBeTracked(t *testing.T) {
	fake := &fakeSession{trackErr: errors.New("no runtime")}
	cmd, _, _ := portCommand()
	if err := runCursorListen(cmd, fake, "cursor:a", "key"); !errors.Is(err, fake.trackErr) {
		t.Fatalf("error = %v", err)
	}
}

type fakeDiagnostics struct {
	sessions   []model.Session
	skipped    int
	listErr    error
	diagErr    error
	report     bus.DiagnosticReport
	pruned     []any
	pruneErr   error
	noticeErr  error
	noticeRuns int
	dir        string
}

func (f *fakeDiagnostics) ListWithWarnings() ([]model.Session, int, error) {
	return f.sessions, f.skipped, f.listErr
}

func (f *fakeDiagnostics) Diagnostics(context.Context) (bus.DiagnosticReport, error) {
	return f.report, f.diagErr
}

func (f *fakeDiagnostics) PruneRetired(dryRun bool, selected []string, retention time.Duration) (bus.PruneReport, error) {
	f.pruned = []any{dryRun, selected, retention}
	return bus.PruneReport{DryRun: dryRun}, f.pruneErr
}

func (f *fakeDiagnostics) NoticeExitedSessions(context.Context) error {
	f.noticeRuns++
	return f.noticeErr
}

func (f *fakeDiagnostics) Dir() string { return f.dir }

func TestRunListWarnsAboutSkippedRecordsAndPrintsSessions(t *testing.T) {
	fake := &fakeDiagnostics{sessions: []model.Session{}, skipped: 2}
	cmd, out, errOut := portCommand()
	if err := runList(cmd, fake, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), "2 session records cannot be read") || strings.TrimSpace(out.String()) != "[]" || fake.noticeRuns != 1 {
		t.Fatalf("stderr %q stdout %q notices %d", errOut.String(), out.String(), fake.noticeRuns)
	}
}

func TestRunListFailsOnAListError(t *testing.T) {
	fake := &fakeDiagnostics{listErr: errors.New("unreadable")}
	cmd, out, _ := portCommand()
	if err := runList(cmd, fake, true); !errors.Is(err, fake.listErr) || out.Len() != 0 || fake.noticeRuns != 0 {
		t.Fatalf("error %v output %q notices %d", err, out.String(), fake.noticeRuns)
	}
}

func TestNoticeDeliveryFailureIsAWarningAndOtherErrorsAreFatal(t *testing.T) {
	cmd, _, errOut := portCommand()
	soft := &fakeDiagnostics{noticeErr: &bus.NoticeDeliveryError{Err: errors.New("native detail")}}
	if err := noticeExitedSessions(cmd, soft); err != nil {
		t.Fatalf("delivery error = %v, want a warning only", err)
	}
	if !strings.Contains(errOut.String(), "completion notice could not be delivered") || strings.Contains(errOut.String(), "native detail") {
		t.Fatalf("stderr = %q", errOut.String())
	}
	hard := &fakeDiagnostics{noticeErr: errors.New("journal broken")}
	if err := noticeExitedSessions(cmd, hard); !errors.Is(err, hard.noticeErr) {
		t.Fatalf("error = %v, want the journal error", err)
	}
}

func TestRunPruneSendsNoticesOnlyAfterARealPrune(t *testing.T) {
	for _, dryRun := range []bool{true, false} {
		fake := &fakeDiagnostics{}
		cmd, out, _ := portCommand()
		if err := runPrune(cmd, fake, dryRun, []string{"claude:a"}, time.Hour, true); err != nil {
			t.Fatal(err)
		}
		want := 1
		if dryRun {
			want = 0
		}
		if fake.noticeRuns != want {
			t.Fatalf("dry run %v: notices %d, want %d", dryRun, fake.noticeRuns, want)
		}
		if fake.pruned[0] != dryRun || fake.pruned[2] != time.Hour {
			t.Fatalf("prune arguments = %v", fake.pruned)
		}
		var report bus.PruneReport
		if err := json.Unmarshal(out.Bytes(), &report); err != nil || report.DryRun != dryRun {
			t.Fatalf("report %q err %v", out.String(), err)
		}
	}
}

func TestRunPruneStopsOnAPruneError(t *testing.T) {
	fake := &fakeDiagnostics{pruneErr: errors.New("locked")}
	cmd, out, _ := portCommand()
	if err := runPrune(cmd, fake, false, nil, 0, true); !errors.Is(err, fake.pruneErr) || fake.noticeRuns != 0 || out.Len() != 0 {
		t.Fatalf("error %v notices %d output %q", err, fake.noticeRuns, out.String())
	}
}
