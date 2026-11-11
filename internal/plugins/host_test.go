package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func TestMain(m *testing.M) {
	readChildTestMain()
	if len(os.Args) == 4 && os.Args[1] == "check-test-child" {
		if err := checkTestChild(os.Args[2], os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) >= 3 && os.Args[1] == "external-test-child" {
		if err := externalTestChild(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 4 && os.Args[1] == ChildCommand && os.Args[2] == "--outbox" {
		if err := referenceOrFakeChild(os.Args[3]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) == 3 && os.Args[1] == "host-test-worker" {
		store, err := bus.NewStore(os.Args[2], adapters.Builtin(), nil)
		if err == nil {
			err = Run(context.Background(), store, os.Args[0], "fake-notifier", io.Discard)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeChild(outbox string) error {
	// Raw FD3 remains open until process exit, including runtime exit handlers.
	if err := pluginfeed.CheckPluginLeaseFD(3); err != nil {
		return err
	}
	state := filepath.Dir(outbox)
	mode, _ := os.ReadFile(filepath.Join(state, ".child-mode"))
	if err := recordFakeChildEnvironment(state); err != nil {
		return err
	}
	if string(mode) == "control-stderr" {
		_, _ = os.Stderr.Write([]byte("\x1b[2Jhidden\x07\u202e\r\xff\nnext\tline"))
		return errors.New("control stderr")
	}
	if string(mode) == "eof-before-hello" {
		_, err := io.Copy(io.Discard, os.Stdin)
		return err
	}
	if string(mode) == "slow-hello" {
		time.Sleep(3 * time.Second) // A cold first launch, well inside helloTimeout.
	}
	if string(mode) == "no-hello" {
		// Outlive the hello deadline so the host's timeout, not an exit, ends it.
		time.Sleep(helloTimeout + 20*time.Second)
		return nil
	}
	version := "v1"
	if string(mode) == "bad-hello" {
		version = "other"
	}
	if err := writeFrame(os.Stdout, frame{Hello: &hello{Version: version}}); err != nil {
		return err
	}
	if string(mode) == "no-read" {
		time.Sleep(10 * time.Second)
		return nil
	}
	if string(mode) == "close-stdout-hold-eof" {
		_ = os.Stdout.Close()
	}
	if string(mode) == "unsolicited" {
		return writeFrame(os.Stdout, frame{Ack: "unexpected"})
	}
	if string(mode) == "oversize" {
		_, err := os.Stdout.Write([]byte(strings.Repeat("x", maxFrame+2) + "\n"))
		return err
	}
	if string(mode) == "stderr" {
		_, _ = os.Stderr.Write([]byte(strings.Repeat("x", 8193)))
	}
	return fakeDeliveries(state, string(mode))
}

func fakeDeliveries(state, mode string) error {
	reader := bufio.NewReaderSize(os.Stdin, maxFrame+1)
	for {
		value, err := readFrame(reader)
		if errors.Is(err, io.EOF) {
			fakeEOF(state, mode)
			return nil
		}
		if err != nil {
			return err
		}
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		// Atomic, so a test that sees the name never reads a half-written file.
		if err := journal.WriteAtomic(filepath.Join(state, ".seen-"+value.ID), raw); err != nil {
			return err
		}
		if mode == "hold" {
			waitPermit(state, ".allow-ack", 10*time.Second)
		}
		if mode == "no-ack" {
			time.Sleep(10 * time.Second)
		}
		id := value.ID
		if mode == "bad-ack" || mode == "bad-ack-hold-eof" {
			id = "wrong"
		}
		if err := writeFrame(os.Stdout, frame{Ack: id}); err != nil {
			return err
		}
	}
}

func fakeEOF(state, mode string) {
	if mode == "cleanup-eof" {
		_ = os.WriteFile(filepath.Join(state, ".eof-cleaned"), []byte("clean"), 0600)
		return
	}
	if mode == "error-eof" {
		os.Exit(17)
	}
	if mode == "hold-eof" || mode == "bad-ack-hold-eof" || mode == "close-stdout-hold-eof" {
		_ = os.WriteFile(filepath.Join(state, ".eof-seen"), []byte("yes"), 0600)
		waitPermit(state, ".release-eof", 3*time.Second)
	}
}

func waitPermit(state, name string, limit time.Duration) {
	until := time.Now().Add(limit)
	for time.Now().Before(until) {
		if _, err := os.Stat(filepath.Join(state, name)); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func hostStore(t *testing.T) (bus.Store, model.Session) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := bus.NewStore(directory, adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := store.Register("codex", "host-test", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").AddPlugin(); err != nil {
		t.Fatal(err)
	}
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").ConfigurePluginFeed([]string{session.ID}); err != nil {
		t.Fatal(err)
	}
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	return store, session
}

func hostData(store bus.Store) string {
	return filepath.Join(store.Dir, "plugins", "fake-notifier.data")
}

func startHost(t *testing.T, store bus.Store) (context.CancelFunc, <-chan error) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, store, binary, "fake-notifier", io.Discard); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("host did not reap its child")
		}
	})
	return cancel, done
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestHostReapsChildOnEOFAndDoesNotInheritSecrets(t *testing.T) {
	store, _ := hostStore(t)
	t.Setenv("ZALKI_CAPABILITY", "DO_NOT_INHERIT")
	lease, err := pluginfeed.OpenPluginLease(filepath.Join(store.Dir, "plugins", "fake-notifier.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := startChild(t.Context(), binary, filepath.Join(hostData(store), "outbox"), lease)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.stop(nil) })
	if value, err := c.receive(2 * time.Second); err != nil || value.Hello == nil {
		t.Fatal("hello", value, err)
	}
	if err := c.stop(nil); err != nil || c.waitErr != nil {
		t.Fatal("EOF did not stop child cleanly", err, c.waitErr)
	}
	raw, err := os.ReadFile(filepath.Join(hostData(store), ".child-env"))
	if err != nil || strings.Contains(string(raw), "DO_NOT_INHERIT") {
		t.Fatal("child inherited credentials", string(raw), err)
	}
	actual, err := os.ReadFile(filepath.Join(hostData(store), ".child-cwd"))
	expected, resolveErr := filepath.EvalSymlinks(hostData(store))
	if err != nil || resolveErr != nil || string(actual) != expected {
		t.Fatal("child cwd is not its persistent data directory", string(actual), expected, err, resolveErr)
	}

}

// A plugin that needs a few seconds before its hello is accepted: the deadline
// is helloTimeout, not the old 2 s.
func TestHostAcceptsASlowHello(t *testing.T) {
	store, _ := hostStore(t)
	if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte("slow-hello"), 0600); err != nil {
		t.Fatal(err)
	}
	_, done := startHost(t, store)
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	until := time.Now().Add(helloTimeout + 5*time.Second)
	for time.Now().Before(until) {
		select {
		case err := <-done:
			t.Fatal("host stopped on a slow but valid hello", err)
		default:
		}
		if status, err := feed.PluginStatus(); err == nil && !status.GapPending {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("slow hello was never accepted")
}

func TestHostStopsOnChildFailures(t *testing.T) {
	for _, mode := range []string{"bad-hello", "unsolicited", "oversize", "bad-ack", "stderr", "no-hello", "no-ack"} {
		t.Run(mode, func(t *testing.T) {
			store, _ := hostStore(t)
			if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte(mode), 0600); err != nil {
				t.Fatal(err)
			}
			_, done := startHost(t, store)
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("child failure was hidden")
				}
			case <-time.After(helloTimeout + 10*time.Second):
				t.Fatal("child failure did not terminate host")
			}
		})
	}
}

func TestHostCrashRetainsLeaseUntilChildExits(t *testing.T) {
	store, _ := hostStore(t)
	if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte("hold-eof"), 0600); err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	worker := exec.CommandContext(t.Context(), binary, "host-test-worker", store.Dir)
	if err := worker.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = worker.Process.Kill(); _, _ = worker.Process.Wait() })
	eventually(t, func() bool {
		s, err := pluginfeed.NewPluginFeed(store, "fake-notifier").PluginStatus()
		return err == nil && !s.GapPending
	})
	if err := worker.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = worker.Wait()
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(hostData(store), ".eof-seen")); return err == nil })
	if lease, err := pluginfeed.OpenPluginLease(filepath.Join(store.Dir, "plugins", "fake-notifier.lock")); err == nil {
		_ = lease.Close()
		t.Fatal("parent death allowed a second child while the first held FD3")
	}
	if err := os.WriteFile(filepath.Join(hostData(store), ".release-eof"), []byte("yes"), 0600); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		lease, err := pluginfeed.OpenPluginLease(filepath.Join(store.Dir, "plugins", "fake-notifier.lock"))
		if err != nil {
			return false
		}
		_ = lease.Close()
		return true
	})
}

func TestHostPendingGapSurvivesAnotherLossAndRestart(t *testing.T) {
	for _, reason := range []string{"retention", "epoch"} {
		t.Run(reason, func(t *testing.T) { pendingGapRestart(t, reason) })
	}
}

func pendingGapRestart(t *testing.T, reason string) {
	t.Helper()
	store, session := hostStore(t)
	initial, err := pluginfeed.NewPluginFeed(store, "fake-notifier").NextPluginFeed()
	if err != nil {
		t.Fatal(err)
	}
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").AckPluginFeed(initial); err != nil {
		t.Fatal(err)
	}
	seedLaterHistory(t, store, session, "retention")
	gap, err := pluginfeed.NewPluginFeed(store, "fake-notifier").NextPluginFeed()
	if err != nil || gap.Gap != "retention" {
		t.Fatal(gap, err)
	}
	if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte("hold"), 0600); err != nil {
		t.Fatal(err)
	}
	stop, done := startHost(t, store)
	seen := filepath.Join(hostData(store), ".seen-"+gap.ID)
	eventually(t, func() bool { _, err := os.Stat(seen); return err == nil })
	seedLaterHistory(t, store, session, reason)
	stop()
	<-done
	if err := os.Remove(seen); err != nil {
		t.Fatal(err)
	}
	startHost(t, store)
	eventually(t, func() bool { _, err := os.Stat(seen); return err == nil })
	assertSeenGap(t, seen, "retention")
	if err := os.WriteFile(filepath.Join(hostData(store), ".allow-ack"), []byte("yes"), 0600); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return laterGapSeen(hostData(store), gap.ID, reason) })
	eventually(t, func() bool {
		s, err := pluginfeed.NewPluginFeed(store, "fake-notifier").PluginStatus()
		return err == nil && s.LastGap == reason && !s.GapPending
	})
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		d, err := pluginfeed.NewPluginFeed(store, "fake-notifier").NextPluginFeed()
		return err == nil && d.ID == ""
	})
}

func seedLaterHistory(t *testing.T, store bus.Store, session model.Session, reason string) {
	t.Helper()
	var metadata struct {
		Epoch string `json:"epoch"`
		Last  int    `json:"last_sequence,string"`
	}
	if err := journal.ReadJSON(filepath.Join(store.Dir, "events/meta.json"), 128<<10, &metadata); err != nil {
		t.Fatal(err)
	}
	first := metadata.Last + 100
	if reason == "epoch" {
		metadata.Epoch, first = "00000000-0000-4000-8000-000000009999", 1
	}
	directory := filepath.Join(store.Dir, "events/records")
	names, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if err := os.Remove(filepath.Join(directory, name.Name())); err != nil {
			t.Fatal(err)
		}
	}
	var total int
	for n := first; n < first+2; n++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012x", n)
		event := model.Event{ID: id, TransactionID: id, Type: "session.state_changed", OccurredAt: "2026-11-08T00:00:00Z",
			Session: model.EventSession{ID: session.ID, Generation: session.Generation, Previous: model.StateBusy, State: model.StateReady}}
		raw, err := journal.EncodeJSON(map[string]any{"epoch": metadata.Epoch, "sequence": strconv.Itoa(n), "event": event}, 4096)
		if err != nil {
			t.Fatal(err)
		}
		total += len(raw)
		if err := journal.WriteAtomic(filepath.Join(directory, fmt.Sprintf("%020d.json", n)), raw); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := journal.EncodeJSON(map[string]any{"epoch": metadata.Epoch, "first_sequence": strconv.Itoa(first), "last_sequence": strconv.Itoa(first + 1), "count": 2, "bytes": total}, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(store.Dir, "events/meta.json"), raw); err != nil {
		t.Fatal(err)
	}
}

func TestHostConsumesOneOfferAndHonorsDisable(t *testing.T) {
	store, session := hostStore(t)
	_, done := startHost(t, store)
	eventually(t, func() bool {
		s, err := pluginfeed.NewPluginFeed(store, "fake-notifier").PluginStatus()
		return err == nil && !s.GapPending
	})
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		d, err := pluginfeed.NewPluginFeed(store, "fake-notifier").NextPluginFeed()
		return err == nil && d.ID == ""
	})
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("disabled host stayed alive")
	}
}

func TestHostRequiresEnableAndRefusesSecondChild(t *testing.T) {
	store, _ := hostStore(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := Run(t.Context(), store, binary, "fake-notifier", io.Discard); err == nil {
		t.Fatal("disabled plugin launched")
	}
	if _, err := os.Stat(filepath.Join(hostData(store), ".child-env")); !os.IsNotExist(err) {
		t.Fatal("disabled host executed child", err)
	}
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	startHost(t, store)
	eventually(t, func() bool {
		s, err := pluginfeed.NewPluginFeed(store, "fake-notifier").PluginStatus()
		return err == nil && !s.GapPending
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := Run(ctx, store, binary, "fake-notifier", io.Discard); err == nil {
		t.Fatal("second host was permitted")
	}
}

func TestLeaseVisibleAfterFailureDoesNotPermitHandoff(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cut requires ordinary user")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "child.lock")
	t.Cleanup(func() { _ = os.Chmod(directory, 0700) })
	if err := os.Chmod(directory, 0300); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		file, err := pluginfeed.OpenPluginLease(path)
		if file != nil || !errors.Is(err, os.ErrPermission) {
			t.Fatal("unconfirmed lease allowed handoff", file, err)
		}
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	file, err := pluginfeed.OpenPluginLease(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
}

func laterGapSeen(state, before, reason string) bool {
	names, err := os.ReadDir(state)
	if err != nil {
		return false
	}
	for _, name := range names {
		if !strings.HasPrefix(name.Name(), ".seen-") || name.Name() == ".seen-"+before {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(state, name.Name()))
		var value frame
		if err == nil && json.Unmarshal(raw, &value) == nil && value.Event == nil && value.Gap == reason {
			return true
		}
	}
	return false
}

func assertSeenGap(t *testing.T, path, reason string) {
	t.Helper()
	var first frame
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &first) != nil {
		t.Fatal("missing pending gap", err)
	}
	if first.Event != nil || first.Gap != reason {
		t.Fatal("pending gap replaced or skipped", first)
	}
}

type cancelErrorWriter struct {
	cancel context.CancelFunc
	err    error
}

func (w cancelErrorWriter) Write([]byte) (int, error) {
	w.cancel()
	return 0, w.err
}

func TestHostPreservesWriterFailureDuringCancellation(t *testing.T) {
	store, _ := hostStore(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	want := errors.New("owner output failed")
	err = Run(ctx, store, binary, "fake-notifier", cancelErrorWriter{cancel, want})
	if !errors.Is(err, want) {
		t.Fatal("shutdown hid the observed owner output failure", err)
	}
}

func TestHostPreservesProtocolFailureDuringCancellation(t *testing.T) {
	store, _ := hostStore(t)
	if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte("bad-ack-hold-eof"), 0600); err != nil {
		t.Fatal(err)
	}
	cancel, done := startHost(t, store)
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(hostData(store), ".eof-seen")); return err == nil })
	// EOF proves the host already rejected the ACK and entered child.stop.
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "invalid ACK") {
			t.Fatal("shutdown hid the observed child protocol failure", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host did not reap after the observed protocol failure")
	}
}

func TestHostCancellationReapsCleanly(t *testing.T) {
	store, _ := hostStore(t)
	cancel, done := startHost(t, store)
	eventually(t, func() bool {
		s, err := pluginfeed.NewPluginFeed(store, "fake-notifier").PluginStatus()
		return err == nil && !s.GapPending
	})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("ordinary cancellation was not clean", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled host did not reap")
	}
	lease, err := pluginfeed.OpenPluginLease(filepath.Join(store.Dir, "plugins", "fake-notifier.lock"))
	if err != nil {
		t.Fatal("cancelled host retained its lease", err)
	}
	_ = lease.Close()
}

func TestHostPreservesObservedEOFDuringCancellation(t *testing.T) {
	store, _ := hostStore(t)
	if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte("close-stdout-hold-eof"), 0600); err != nil {
		t.Fatal(err)
	}
	cancel, done := startHost(t, store)
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(hostData(store), ".eof-seen")); return err == nil })
	// The host saw unsolicited EOF before closing stdin and reaching this fence.
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Fatal("shutdown hid the previously observed stream EOF", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("host did not reap after unsolicited EOF")
	}
}

func TestNamedPluginRemoveWaitsForReapedChild(t *testing.T) {
	store, _ := hostStore(t)
	feed := pluginfeed.NewPluginFeed(store, "fake-notifier")
	if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte("hold-eof"), 0600); err != nil {
		t.Fatal(err)
	}
	_, done := startHost(t, store)
	eventually(t, func() bool { status, err := feed.PluginStatus(); return err == nil && !status.GapPending })
	if err := feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(hostData(store), ".eof-seen")); return err == nil })
	if err := feed.RemovePlugin(); err == nil {
		t.Fatal("removed installation while child still held FD3")
	}
	status, err := feed.PluginStatus()
	if err != nil || status.ID == "" {
		t.Fatal("failed remove changed record", status, err)
	}
	if err := os.WriteFile(filepath.Join(hostData(store), ".release-eof"), []byte("yes"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("child did not reap")
	}
	if err := feed.RemovePlugin(); err != nil {
		t.Fatal("remove refused reaped child", err)
	}
	if _, err := os.Lstat(hostData(store)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("remove retained child data", err)
	}
}

func recordFakeChildEnvironment(state string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(state, ".child-cwd"), []byte(cwd), 0600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(state, ".child-env"), []byte(strings.Join(os.Environ(), "\n")), 0600)
}

// Plugin stderr reaches the owner's terminal inside the host error, so control
// sequences must arrive escaped while line breaks and tabs stay real.
func TestHostEscapesChildStderr(t *testing.T) {
	store, _ := hostStore(t)
	if err := os.WriteFile(filepath.Join(hostData(store), ".child-mode"), []byte("control-stderr"), 0600); err != nil {
		t.Fatal(err)
	}
	_, done := startHost(t, store)
	select {
	case err := <-done:
		text := fmt.Sprint(err)
		if err == nil || !strings.Contains(text, `\x1b[2Jhidden\a\u202e\r\xff`+"\nnext\tline") {
			t.Fatalf("stderr not escaped as expected: %q", text)
		}
		if strings.ContainsAny(text, "\x1b\x07\u202e\r") {
			t.Fatalf("raw control character reached the error: %q", text)
		}
	case <-time.After(helloTimeout + 10*time.Second):
		t.Fatal("child failure did not terminate host")
	}
}

func TestPlainStderr(t *testing.T) {
	for in, want := range map[string]string{
		"plain text\twith tab\n": "plain text\twith tab\n",
		"\x1b]0;title\x07":       `\x1b]0;title\a`,
		"caf\u00e9 \u202eevil":   "caf\u00e9 " + `\u202eevil`,
		"bad \xff byte":          `bad \xff byte`,
		"zero\u200bwidth":        `zero\u200bwidth`,
	} {
		if got := plainStderr([]byte(in)); got != want {
			t.Errorf("plainStderr(%q) = %q, want %q", in, got, want)
		}
	}
}
