package plugins

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func referenceChild(t *testing.T, state string) (*child, func()) {
	t.Helper()
	return referenceChildMode(t, state, "reference")
}

func referenceChildMode(t *testing.T, state, mode string) (*child, func()) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(state, ".child-mode"), []byte(mode), 0600); err != nil {
		t.Fatal(err)
	}
	lease, err := pluginfeed.OpenPluginLease(filepath.Join(state, "child.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	c, err := startChild(t.Context(), binary, filepath.Join(state, "outbox"), lease)
	if err != nil {
		_ = lease.Close()
		t.Fatal(err)
	}
	stop := func() { _ = c.stop(nil); _ = lease.Close() }
	t.Cleanup(stop)
	if value, err := c.receive(2 * time.Second); err != nil || value.Hello == nil || value.Hello.Version != "v1" {
		t.Fatal("reference hello failed", value, err)
	}
	return c, stop
}

func TestReferenceChildReplaysWithoutDuplicateAppend(t *testing.T) {
	state := notifierTemp(t)
	event := testEvent(1)
	value := frame{ID: event.ID, Event: &event}
	first, stop := referenceChild(t, state)
	if err := first.exchange(value); err != nil {
		t.Fatal(err)
	}
	stop()
	second, _ := referenceChild(t, state)
	if err := second.exchange(value); err != nil {
		t.Fatal(err)
	}
	names, err := os.ReadDir(filepath.Join(state, "outbox"))
	if err != nil || len(names) != 1 || names[0].Name() != event.ID+".json" {
		t.Fatal("outbox replay duplicated output", names, err)
	}
}

func TestReferenceChildWithholdsACKAfterUnknownRenameOutcome(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cut requires ordinary user")
	}
	state := notifierTemp(t)
	outbox := filepath.Join(state, "outbox")
	if err := prepareOutbox(outbox); err != nil {
		t.Fatal(err)
	}
	event := testEvent(1)
	raw, err := journal.EncodeJSON(event, 4096)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outbox, 0700) })
	if err := os.Chmod(outbox, 0300); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(outbox, event.ID+".json")
	if err := fsutil.WriteAtomic(target, raw); !errors.Is(err, os.ErrPermission) {
		t.Fatal("did not reach post-rename sync cut", err)
	}
	if visible, err := journal.ReadBytes(target, 4096); err != nil || !bytes.Equal(visible, raw) {
		t.Fatal("cut did not publish complete live bytes", err)
	}
	value := frame{ID: event.ID, Event: &event}
	for range 2 {
		c, stop := referenceChild(t, state)
		if err := c.exchange(value); err == nil {
			t.Fatal("visible but unconfirmed outbox was ACKed")
		}
		stop()
	}
	if err := os.Chmod(outbox, 0700); err != nil {
		t.Fatal(err)
	}
	// A crash may retain the namespace preceding that failed directory sync.
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	c, _ := referenceChild(t, state)
	if err := c.exchange(value); err != nil {
		t.Fatal("durable replay did not ACK", err)
	}
	if visible, err := journal.ReadBytes(target, 4096); err != nil || !bytes.Equal(visible, raw) {
		t.Fatal("ACK preceded complete output", err)
	}
}

func TestOutboxParentLinkMustBeConfirmedOnRetry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cut requires ordinary user")
	}
	state := notifierTemp(t)
	outbox := filepath.Join(state, "outbox")
	t.Cleanup(func() { _ = os.Chmod(state, 0700) })
	if err := os.Chmod(state, 0300); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := prepareOutbox(outbox); !errors.Is(err, os.ErrPermission) {
			t.Fatal("visible directory skipped parent confirmation", err)
		}
	}
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(outbox); err != nil {
		t.Fatal(err)
	}
	if err := prepareOutbox(outbox); err != nil {
		t.Fatal(err)
	}
}

func cutReference(outbox, mode string) error {
	if err := pluginfeed.CheckPluginLeaseFD(3); err != nil {
		return err
	}
	if err := prepareOutbox(outbox); err != nil {
		return err
	}
	if err := writeFrame(os.Stdout, frame{Hello: &hello{Version: "v1"}}); err != nil {
		return err
	}
	value, err := readFrame(bufio.NewReaderSize(os.Stdin, maxFrame+1))
	if err != nil {
		return err
	}
	marker := ".notify-durable"
	if mode == "reference-cut-stage" {
		if err := os.WriteFile(filepath.Join(outbox, ".zalki-config-test-interrupted"), []byte("partial"), 0600); err != nil {
			return err
		}
		marker = ".stage-visible"
	} else if err := notify(outbox, value); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(outbox), marker), []byte("yes"), 0600); err != nil {
		return err
	}
	waitPermit(filepath.Dir(outbox), ".never-allow-ack", 10*time.Second)
	return nil
}

func TestReferenceRecoversFromKillBeforeACK(t *testing.T) {
	for _, phase := range []string{"stage", "after-write"} {
		t.Run(phase, func(t *testing.T) { referenceKillReplay(t, phase) })
	}
}

func referenceKillReplay(t *testing.T, phase string) {
	t.Helper()
	state := notifierTemp(t)
	c, stop := referenceChildMode(t, state, "reference-cut-"+phase)
	event := testEvent(1)
	value := frame{ID: event.ID, Event: &event}
	if err := c.input.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(c.input, value); err != nil {
		t.Fatal(err)
	}
	marker := ".notify-durable"
	if phase == "stage" {
		marker = ".stage-visible"
	}
	eventually(t, func() bool { _, err := os.Stat(filepath.Join(state, marker)); return err == nil })
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.receive(time.Second); err == nil {
		t.Fatal("killed child ACKed")
	}
	stop()
	replay, _ := referenceChild(t, state)
	if err := replay.exchange(value); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(state, "outbox"))
	if err != nil || len(files) != 1 || files[0].Name() != event.ID+".json" {
		t.Fatal("kill replay lost or duplicated event", files, err)
	}
}

func TestReferenceReadOnlyOutboxWithholdsACK(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cut requires ordinary user")
	}
	state := notifierTemp(t)
	outbox := filepath.Join(state, "outbox")
	if err := prepareOutbox(outbox); err != nil {
		t.Fatal(err)
	}
	event := testEvent(1)
	value := frame{ID: event.ID, Event: &event}
	t.Cleanup(func() { _ = os.Chmod(outbox, 0700) })
	if err := os.Chmod(outbox, 0500); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		c, stop := referenceChild(t, state)
		if err := c.exchange(value); err == nil {
			t.Fatal("read-only outbox ACKed failed write")
		}
		stop()
	}
	if err := os.Chmod(outbox, 0700); err != nil {
		t.Fatal(err)
	}
	c, _ := referenceChild(t, state)
	if err := c.exchange(value); err != nil {
		t.Fatal(err)
	}
}

func referenceOrFakeChild(outbox string) error {
	mode, _ := os.ReadFile(filepath.Join(filepath.Dir(outbox), ".child-mode"))
	if string(mode) == "reference" {
		return RunNotifier(context.Background(), os.Stdin, os.Stdout, outbox)
	}
	if strings.HasPrefix(string(mode), "reference-cut-") {
		return cutReference(outbox, string(mode))
	}
	return fakeChild(outbox)
}

func TestReferenceReadOnlyReplayWithholdsACK(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cut requires ordinary user")
	}
	state := notifierTemp(t)
	outbox := filepath.Join(state, "outbox")
	if err := prepareOutbox(outbox); err != nil {
		t.Fatal(err)
	}
	event := testEvent(1)
	value := frame{ID: event.ID, Event: &event}
	if err := notify(outbox, value); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outbox, 0700) })
	if err := os.Chmod(outbox, 0500); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		c, stop := referenceChild(t, state)
		if err := c.exchange(value); err == nil {
			t.Fatal("matching replay ACKed without a durable rewrite")
		}
		stop()
	}
	if err := os.Chmod(outbox, 0700); err != nil {
		t.Fatal(err)
	}
	c, _ := referenceChild(t, state)
	if err := c.exchange(value); err != nil {
		t.Fatal("matching replay did not recover", err)
	}
}

func TestReferenceStagingCleanupFailureWithholdsACK(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cut requires ordinary user")
	}
	state := notifierTemp(t)
	outbox := filepath.Join(state, "outbox")
	if err := prepareOutbox(outbox); err != nil {
		t.Fatal(err)
	}
	stage := filepath.Join(outbox, ".zalki-config-interrupted")
	if err := os.WriteFile(stage, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(outbox, 0700) })
	if err := os.Chmod(outbox, 0500); err != nil {
		t.Fatal(err)
	}
	event := testEvent(1)
	value := frame{ID: event.ID, Event: &event}
	for range 2 {
		c, stop := referenceChild(t, state)
		if err := c.exchange(value); err == nil {
			t.Fatal("failed staging cleanup was ACKed")
		}
		stop()
		if _, err := os.Stat(stage); err != nil {
			t.Fatal("failed cleanup lost its stage", err)
		}
	}
	if err := os.Chmod(outbox, 0700); err != nil {
		t.Fatal(err)
	}
	c, _ := referenceChild(t, state)
	if err := c.exchange(value); err != nil {
		t.Fatal("staging cleanup did not recover", err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("successful replay left a stage", err)
	}
}

func TestReferenceChildStopsCleanlyOnParentEOF(t *testing.T) {
	c, _ := referenceChild(t, notifierTemp(t))
	if err := c.stop(nil); err != nil {
		t.Fatal("reference child failed after parent EOF", err)
	}
}

func referenceBinary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("ZALKI_TEST_BINARY"); path != "" {
		return path
	}
	path := filepath.Join(t.TempDir(), "zalki")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-buildvcs=false", "-o", path, "../../cmd/zalki")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatal("build reference binary", string(raw), err)
	}
	return path
}

func referenceCommand(t *testing.T, ctx context.Context, binary string, store bus.Store) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(ctx, binary, "--state", store.Dir, "--log-level", "off", "plugins", "run", "fake-notifier")
	cmd.Env = []string{"HOME=" + notifierTemp(t)}
	return cmd
}

func seedFullOutbox(t *testing.T, path string) {
	t.Helper()
	if err := prepareOutbox(path); err != nil {
		t.Fatal(err)
	}
	for n := 1; n <= 256; n++ {
		event := testEvent(n)
		raw, err := journal.EncodeJSON(event, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, event.ID+".json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPluginCommandBinaryFullOutboxRecovery(t *testing.T) {
	store, session := hostStore(t)
	gap, err := pluginfeed.NewPluginFeed(store, "fake-notifier").NextPluginFeed()
	if err != nil || gap.Gap != "enabled" {
		t.Fatal("initial gap", gap, err)
	}
	if err := pluginfeed.NewPluginFeed(store, "fake-notifier").AckPluginFeed(gap); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(session.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	pending, err := pluginfeed.NewPluginFeed(store, "fake-notifier").NextPluginFeed()
	if err != nil || pending.Event == nil {
		t.Fatal("missing pending state change", pending, err)
	}
	outbox := filepath.Join(hostData(store), "outbox")
	seedFullOutbox(t, outbox)
	binary := referenceBinary(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := referenceCommand(t, ctx, binary, store)
	if raw, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(raw), "outbox is full") {
		t.Fatal("full outbox did not fail explicitly", string(raw), err)
	}
	again, err := pluginfeed.NewPluginFeed(store, "fake-notifier").NextPluginFeed()
	if err != nil || again.ID != pending.ID {
		t.Fatal("failed notification advanced core cursor", again, err)
	}
	if err := os.Remove(filepath.Join(outbox, eventID(1)+".json")); err != nil {
		t.Fatal(err)
	}
	runReferenceUntilConsumed(t, store, binary)
	assertReferenceRecovery(t, outbox, *pending.Event)
}

func assertReferenceRecovery(t *testing.T, outbox string, event model.Event) {
	t.Helper()
	raw, err := journal.ReadBytes(filepath.Join(outbox, event.ID+".json"), 4096)
	want, encodeErr := journal.EncodeJSON(event, 4096)
	if err != nil || encodeErr != nil || !bytes.Equal(raw, want) {
		t.Fatal("recovered cursor preceded complete outbox event", err, encodeErr)
	}
	if names, err := os.ReadDir(outbox); err != nil || len(names) != 256 {
		t.Fatal("recovery exceeded the outbox cap", len(names), err)
	}
}

func runReferenceUntilConsumed(t *testing.T, store bus.Store, binary string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	cmd := referenceCommand(t, ctx, binary, store)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("reference command did not exit")
		}
	})
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
			t.Fatal("reference command failed during clean disable", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reference command did not honor disable")
	}
}
