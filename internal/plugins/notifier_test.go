package plugins

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestNotifierCancellationPreservesReadFailure(t *testing.T) {
	for _, raw := range []string{"{broken}\n", "{\"unknown\":true}\n", "{\"id\":"} {
		t.Run(raw, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			input := &cancelingInput{Reader: strings.NewReader(raw), cancel: cancel}
			if err := runNotifierLoop(ctx, input, io.Discard, notifierTemp(t)); err == nil {
				t.Fatal("cancellation hid a malformed or truncated frame")
			}
		})
	}
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		input := &cancelingInput{err: os.ErrClosed, cancel: func() {}}
		if canceled {
			input.cancel = cancel
		}
		err := runNotifierLoop(ctx, input, io.Discard, notifierTemp(t))
		cancel()
		if (err == nil) != canceled {
			t.Fatalf("closed input: canceled=%v, error=%v", canceled, err)
		}
	}
}

func TestNotifierCancellationPreservesWriteFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := io.NopCloser(strings.NewReader(fmt.Sprintf("{\"id\":%q,\"gap\":\"enabled\"}\n", eventID(1))))
	err := runNotifierLoop(ctx, input, cancelingOutput{cancel: cancel}, notifierTemp(t))
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("cancellation hid an ACK write failure", err)
	}
}

func TestNotifierDistinguishesEmptyFrameFromEOF(t *testing.T) {
	for _, raw := range []string{"", "\n", " \t\n"} {
		for _, canceled := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			input := &cancelingInput{Reader: strings.NewReader(raw), cancel: func() {}}
			if canceled {
				input.cancel = cancel
			}
			err := runNotifierLoop(ctx, input, io.Discard, notifierTemp(t))
			cancel()
			if raw == "" && err != nil {
				t.Fatalf("clean stream EOF: canceled=%v, error=%v", canceled, err)
			}
			if raw != "" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("empty frame %q: canceled=%v, error=%v", raw, canceled, err)
			}
		}
	}
}

type cancelingInput struct {
	*strings.Reader
	cancel context.CancelFunc
	err    error
}

func (r *cancelingInput) Read(p []byte) (int, error) {
	r.cancel()
	if r.err != nil {
		return 0, r.err
	}
	return r.Reader.Read(p)
}

func (*cancelingInput) Close() error { return nil }

type cancelingOutput struct{ cancel context.CancelFunc }

func (w cancelingOutput) Write([]byte) (int, error) {
	w.cancel()
	return 0, io.ErrClosedPipe
}

func eventID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012x", n) }

func testEvent(n int) model.Event {
	return model.Event{ID: eventID(n), Type: "session.state_changed", OccurredAt: "2026-11-08T00:00:00Z",
		TransactionID: eventID(n + 10000), Session: model.EventSession{ID: "codex:" + eventID(9000),
			Generation: eventID(9001), Previous: model.StateBusy, State: model.StateReady}}
}

func TestNotifierGapRegistrationAndReplay(t *testing.T) {
	path := notifierTemp(t)
	if err := prepareOutbox(path); err != nil {
		t.Fatal(err)
	}
	if err := notify(path, frame{ID: eventID(1), Gap: "retention"}); err != nil {
		t.Fatal(err)
	}
	event := testEvent(2)
	event.Type = "session.registered"
	if err := notify(path, frame{ID: event.ID, Event: &event}); err != nil {
		t.Fatal(err)
	}
	if names, err := os.ReadDir(path); err != nil || len(names) != 0 {
		t.Fatal("gap/registration created historical notification", names, err)
	}
	event = testEvent(3)
	value := frame{ID: event.ID, Event: &event}
	for range 2 {
		if err := notify(path, value); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := journal.ReadBytes(filepath.Join(path, event.ID+".json"), 4096)
	if err != nil || !bytes.Contains(raw, []byte(`"state": "ready"`)) {
		t.Fatal("missing local notification", string(raw), err)
	}
	if names, err := os.ReadDir(path); err != nil || len(names) != 1 {
		t.Fatal("replay duplicated outbox", names, err)
	}
	event.OccurredAt = "2026-11-08T00:00:01Z"
	if err := notify(path, value); err == nil {
		t.Fatal("same event ID overwrote different data")
	}
}

func TestNotifierOutboxCapsAndRecovery(t *testing.T) {
	path := notifierTemp(t)
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
	event := testEvent(257)
	if err := notify(path, frame{ID: event.ID, Event: &event}); err == nil {
		t.Fatal("outbox accepted a 257th event")
	}
	event = testEvent(1)
	if err := notify(path, frame{ID: event.ID, Event: &event}); err != nil {
		t.Fatal("full outbox refused idempotent replay", err)
	}
	stage := filepath.Join(path, ".zalki-config-interrupted")
	if err := os.WriteFile(stage, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if count, err := outboxCount(path); err != nil || count != 256 {
		t.Fatal("interrupted write was not recovered", count, err)
	}
	if _, err := os.Stat(stage); !os.IsNotExist(err) {
		t.Fatal("staging file remained", err)
	}
}

func TestNotifierRejectsUnsafeOutput(t *testing.T) {
	for _, mode := range []string{"symlink-directory", "public-directory", "symlink-event", "oversize"} {
		t.Run(mode, func(t *testing.T) {
			path, event := unsafeOutboxCase(t, mode)
			prepare := prepareOutbox(path)
			if prepare == nil && notify(path, frame{ID: event.ID, Event: &event}) == nil {
				t.Fatal("unsafe outbox accepted")
			}
		})
	}
}

func notifierTemp(t *testing.T) string {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func unsafeOutboxCase(t *testing.T, mode string) (string, model.Event) {
	t.Helper()
	path := notifierTemp(t)
	event := testEvent(1)
	switch mode {
	case "symlink-directory":
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(path, link); err != nil {
			t.Fatal(err)
		}
		path = link
	case "public-directory":
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
	case "symlink-event":
		if err := os.Symlink(filepath.Join(t.TempDir(), "other"), filepath.Join(path, event.ID+".json")); err != nil {
			t.Fatal(err)
		}
	case "oversize":
		event.Session.ID = strings.Repeat("a", 5000) + ":" + eventID(9)
	}
	return path, event
}

func TestNotifierMatchingReplayReplacesUnsyncedBytes(t *testing.T) {
	path := notifierTemp(t)
	event := testEvent(1)
	raw, err := journal.EncodeJSON(event, 4096)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(path, event.ID+".json")
	if err := os.WriteFile(target, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := notify(path, frame{ID: event.ID, Event: &event}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(target)
	if err != nil || os.SameFile(before, after) {
		t.Fatal("matching live bytes skipped the atomic file rewrite", err)
	}
	got, err := journal.ReadBytes(target, 4096)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("replay changed the complete event bytes", err)
	}
}
