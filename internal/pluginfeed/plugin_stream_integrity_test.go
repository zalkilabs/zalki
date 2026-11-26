package pluginfeed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/platform"
)

type streamIntegrityHandler struct {
	slog.Handler
	feed      PluginFeed
	lockFree  bool
	storeFree bool
	calls     int
}

func TestStreamIntegrityEpisodeDeduplicatesUntilRecovery(t *testing.T) {
	store := fixtureStore(t)
	feed := NewPluginFeed(store, "fake-notifier")
	var output bytes.Buffer
	feed.store = feed.store.With(bus.WithLogger(slog.New(slog.NewJSONHandler(&output, nil))))
	cause := errors.New("private body/path must never be formatted")
	for range 3 {
		feed.stream.mu.Lock()
		feed.noteStreamReadFailure("not-an-id", cause)
		feed.finishStreamStep()
	}
	if strings.Count(output.String(), "plugin.feed.integrity") != 1 {
		t.Fatal("F_INTEGRITY_ONE_DIAGNOSTIC_PER_EPISODE")
	}
	state := pluginStreamState{}
	feed.captureStreamCut(&state, bus.EventWindow{})
	feed.stream.mu.Lock()
	feed.noteStreamReadFailure("not-an-id", cause)
	feed.finishStreamStep()
	if strings.Count(output.String(), "plugin.feed.integrity") != 2 || strings.Contains(output.String(), cause.Error()) {
		t.Fatal("F_INTEGRITY_NEW_EPISODE_AFTER_SUCCESSFUL_CUT")
	}
}

func (h *streamIntegrityHandler) Handle(ctx context.Context, record slog.Record) error {
	lockFree := h.feed.stream.mu.TryLock()
	if lockFree {
		h.feed.stream.mu.Unlock()
	}
	storeFree := false
	fd, err := platform.OpenLockFile(filepath.Join(h.feed.store.Dir(), "lock"), false)
	if err == nil {
		if err = platform.LockExclusive(fd, true); err == nil {
			storeFree = true
			_ = platform.Unlock(fd)
		}
		_ = platform.CloseLockFile(fd)
	}
	if h.calls == 0 {
		h.lockFree, h.storeFree = lockFree, storeFree
	} else {
		h.lockFree = h.lockFree && lockFree
		h.storeFree = h.storeFree && storeFree
	}
	h.calls++
	return h.Handler.Handle(ctx, record)
}

func TestStreamIntegrityGapRetainsCursorAndLogsOnce(t *testing.T) {
	for _, mode := range []string{"malformed", "malformed-after-filtered", "wrong-epoch", "oversize", "decoded"} {
		t.Run(mode, func(t *testing.T) { checkStreamIntegrityGapCase(t, mode) })
	}
}

type streamIntegrityCase struct {
	mode               string
	store              bus.Store
	session            model.Session
	feed               PluginFeed
	launch             *PluginFeedStatus
	before             pluginStreamState
	epoch, eventPath   string
	sequence           uint64
	original, meta     []byte
	event              model.Event
	wantID, wantReason string
}

func newStreamIntegrityCase(t *testing.T, mode string) *streamIntegrityCase {
	t.Helper()
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	before, _, err := feed.loadStreamState(newStreamBudget(t.Context()))
	if err != nil {
		t.Fatal(err)
	}
	if mode == "malformed-after-filtered" {
		foreign, _ := register(t, store, "codex")
		forgetSession(t, store, foreign.ID)
	}
	updateStreamState(t, store, session.ID, model.StateBusy)
	head := eventState(t, store)
	path := filepath.Join(store.Dir(), eventRecordPath(head.Last))
	c := &streamIntegrityCase{mode: mode, store: store, session: session, feed: feed, launch: launch, before: before, epoch: head.Epoch, sequence: head.Last, eventPath: path, original: readCommitFile(t, path), meta: readCommitFile(t, filepath.Join(store.Dir(), eventMetaPath)), wantID: "unknown", wantReason: "invalid_record"}
	if err := bus.WithJournalWrite(store, func(view bus.JournalView) error {
		var err error
		c.event, err = view.EventAt(head.Epoch, head.Last)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func (c *streamIntegrityCase) corrupt(t *testing.T) {
	t.Helper()
	switch c.mode {
	case "malformed", "malformed-after-filtered":
		if err := fsutil.WriteAtomic(c.eventPath, []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
	case "wrong-epoch":
		if err := fsutil.WriteAtomic(c.eventPath, bytes.Replace(c.original, []byte(c.epoch), []byte(model.NewUUID()), 1)); err != nil {
			t.Fatal(err)
		}
	case "oversize":
		large := bytes.Repeat([]byte(" "), maxEventBytes+1)
		copy(large, c.original)
		if !json.Valid(large) {
			t.Fatal("oversize fixture must be valid JSON")
		}
		if err := fsutil.WriteAtomic(c.eventPath, large); err != nil {
			t.Fatal(err)
		}
		c.wantReason = "record_file_rejected"
	case "decoded":
		path, err := sessionTestPath(c.store, c.session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := fsutil.WriteAtomic(path, []byte("{}\n")); err != nil {
			t.Fatal(err)
		}
		c.wantID, c.wantReason = c.event.ID, "membership_unavailable"
	}
}

func checkStreamIntegrityGapCase(t *testing.T, mode string) {
	t.Helper()
	c := newStreamIntegrityCase(t, mode)
	c.corrupt(t)
	if mode != "decoded" {
		if err := bus.WithJournalWrite(c.store, func(view bus.JournalView) error {
			_, err := view.EventAt(c.epoch, c.sequence)
			return err
		}); err == nil {
			t.Fatal("F_INTEGRITY_RAW_EVENT_READER_STILL_ERRORS")
		}
	}
	var output bytes.Buffer
	handler := &streamIntegrityHandler{Handler: slog.NewJSONHandler(&output, nil), feed: c.feed}
	c.feed.store = c.feed.store.With(bus.WithLogger(slog.New(handler)))
	var work *streamBudget
	factory := c.feed.stream.budget
	c.feed.stream.budget = func(ctx context.Context) *streamBudget { work = factory(ctx); return work }
	gap := streamInspect(t, c.feed, c.launch)
	assertStreamIntegrityGap(t, c, gap, work)
	assertStreamIntegrityReset(t, c)
	for range 3 {
		streamInspect(t, c.feed, c.launch)
	}
	assertStreamIntegrityDiagnostics(t, c, handler, output.String())
}

func assertStreamIntegrityGap(t *testing.T, c *streamIntegrityCase, gap PluginDelivery, work *streamBudget) {
	t.Helper()
	if c.mode == "oversize" && (work == nil || work.used[2] != 0 || work.used[4] != 0) {
		t.Fatal("F_INTEGRITY_OVERSIZE_BEFORE_NATIVE_OR_BODY_PROBES")
	}
	if gap.Gap != "unavailable" || gap.ID == "" || gap.Event != nil {
		t.Fatal("F_INTEGRITY_EXPLICIT_UNAVAILABLE_GAP")
	}
	for _, raw := range gap.Frames {
		if len(raw) != 0 {
			t.Fatal("F_INTEGRITY_INSPECT_HAS_NO_EVENT_BYTES")
		}
	}
}

func assertStreamIntegrityReset(t *testing.T, c *streamIntegrityCase) {
	t.Helper()
	after, _, err := c.feed.loadStreamState(newStreamBudget(t.Context()))
	if err != nil || after.Cursor != c.before.Cursor || len(after.Rows) != 0 || len(after.States) != 0 || len(after.Seen) != 0 {
		t.Fatal("F_INTEGRITY_CURSOR_UNCHANGED_AND_EVIDENCE_CLEARED", err)
	}
}

func streamIntegrityDiagnosticLines(t *testing.T, output string) (lines, waits []string) {
	t.Helper()
	for _, raw := range strings.Split(strings.TrimSpace(output), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(raw), &entry) != nil {
			t.Fatal("invalid diagnostic")
		}
		switch entry["msg"] {
		case "plugin.feed.integrity":
			lines = append(lines, raw)
		case "plugin.feed.waiting":
			waits = append(waits, raw)
		default:
			t.Fatal("unexpected diagnostic")
		}
	}
	return lines, waits
}

func assertStreamIntegrityDiagnostics(t *testing.T, c *streamIntegrityCase, handler *streamIntegrityHandler, output string) {
	t.Helper()
	if !handler.lockFree || !handler.storeFree {
		t.Fatal("F_INTEGRITY_LOG_AFTER_ALL_LOCKS_RELEASED")
	}
	lines, waits := streamIntegrityDiagnosticLines(t, output)
	if len(waits) != 1 {
		t.Fatal("F_INTEGRITY_ONE_BOUNDED_WAIT_DIAGNOSTIC")
	}
	assertStreamIntegrityWait(t, waits[0])
	if len(lines) != 1 {
		t.Fatal("F_INTEGRITY_ONE_DIAGNOSTIC_PER_EPISODE", output)
	}
	assertStreamIntegrityRecord(t, lines[0], c.wantID, c.wantReason)
	if strings.Contains(output, c.store.Dir()) || strings.Contains(output, c.session.ID) {
		t.Fatal("F_INTEGRITY_NO_BODY_OR_USER_PATH")
	}
	if !bytes.Equal(c.meta, readCommitFile(t, filepath.Join(c.store.Dir(), eventMetaPath))) {
		t.Fatal("F_INTEGRITY_SOURCE_METADATA_UNMODIFIED")
	}
}

func assertStreamIntegrityWait(t *testing.T, raw string) {
	t.Helper()
	var wait map[string]any
	if json.Unmarshal([]byte(raw), &wait) != nil || wait["level"] != "INFO" || wait["reason"] != "unavailable" || wait["retry_delay_seconds"] != float64(1) || len(wait) != 5 {
		t.Fatal("F_INTEGRITY_WAIT_REASON_DELAY_NO_PRIVATE_DATA")
	}
}

func assertStreamIntegrityRecord(t *testing.T, raw, wantID, wantReason string) {
	t.Helper()
	var line map[string]any
	if err := json.Unmarshal([]byte(raw), &line); err != nil || line["level"] != "INFO" || line["msg"] != "plugin.feed.integrity" || line["event_id"] != wantID || line["reason"] != wantReason || len(line) != 5 {
		t.Fatal("F_INTEGRITY_SAFE_ID_AND_REASON_ONLY", line, err)
	}
}
