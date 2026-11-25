package pluginfeed

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func installFeed(t *testing.T, s bus.Store) {
	t.Helper()
	feed := NewPluginFeed(s, "fake-notifier")
	status, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.ID == "" {
		if err := feed.AddExternalPlugin(testExecutable(t), nil); err != nil {
			t.Fatal(err)
		}
		if err := feed.GrantPluginAccess(PluginAccess{Status: true}); err != nil {
			t.Fatal(err)
		}
	}
}

func enableFeed(t *testing.T, s bus.Store) {
	t.Helper()
	installFeed(t, s)
	if err := NewPluginFeed(s, "fake-notifier").ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").GrantPluginAccess(PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	d := feedNext(t, s)
	if d.Gap != "enabled" || d.Event != nil {
		t.Fatalf("initial delivery = %+v", d)
	}
	feedAck(t, s, d)
}

func feedNext(t *testing.T, s bus.Store) PluginDelivery {
	t.Helper()
	d, err := nextFeed(NewPluginFeed(s, "fake-notifier"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func feedAck(t *testing.T, s bus.Store, d PluginDelivery) {
	t.Helper()
	if err := testFeedAck(NewPluginFeed(s, "fake-notifier"), d); err != nil {
		t.Fatal(err)
	}
}

func feedRecord(t *testing.T, s bus.Store) pluginRecord {
	t.Helper()
	p, _, err := NewPluginFeed(s, "fake-notifier").loadPlugin()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPluginFeedNowReplayAndPrivateReceipt(t *testing.T) {
	store := fixtureStore(t)
	session, key := register(t, store, "codex")
	if _, err := (&bus.Service{Store: store}).SetTask(session.ID, key, "PRIVATE_TASK"); err != nil {
		t.Fatal(err)
	}
	installFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	launch := streamTestLaunch(t, feed)
	gap := streamInspect(t, feed, launch)
	if gap.Gap != "enabled" || gap.ID == "" {
		t.Fatal("expected explicit initial skipped-history gap")
	}
	first := streamAttemptAll(t, feed, launch, gap)
	updateStreamState(t, store, session.ID, model.StateBusy)
	reopened := NewPluginFeed(bus.StoreAt(store.Dir(), bus.WithRegistry(store.Registry())), "fake-notifier")
	captured := streamTestLaunch(t, reopened)
	again := streamInspect(t, reopened, captured)
	if again.ID != gap.ID {
		t.Fatal("attempted gap disappeared across restart")
	}
	replay := streamAttemptAll(t, reopened, captured, again)
	if !bytes.Equal(first[0], replay[0]) {
		t.Fatal("gap replay changed immutable bytes")
	}
	acknowledgeStream(t, reopened, captured, again.ID)
	reset := streamInspect(t, reopened, captured)
	if reset.Gap != "launch" || reset.ID == gap.ID {
		t.Fatal("resumed offer was not followed by fresh launch barrier")
	}
	streamComplete(t, reopened, captured, reset)
	if cut := streamInspect(t, reopened, captured); cut.ID != "" {
		t.Fatal("plain cut exposed a fabricated event")
	}
	updateStreamState(t, store, session.ID, model.StateReady)
	if unseen := streamInspect(t, reopened, captured); unseen.ID != "" {
		t.Fatal("unknown interval bypassed private floor")
	}
	updateStreamState(t, store, session.ID, model.StateBusy)
	offered, fact := streamTake(t, reopened, captured)
	if fact.Event == nil || fact.Event.Session.State != model.StateBusy {
		t.Fatal("later continuous state event lost")
	}
	assertPluginMetadataOnly(t, PluginDelivery{ID: offered.ID, Event: fact.Event}, "PRIVATE_TASK", key, session.CWD, "epoch", "sequence", "position", "prior", "adapter_state")
	assertAttemptedStreamRestart(t, store, offered.ID, *fact.Event)
}

func assertAttemptedStreamRestart(t *testing.T, store bus.Store, expectedID string, expected model.Event) {
	t.Helper()
	last := NewPluginFeed(store, "fake-notifier")
	lastLaunch := streamTestLaunch(t, last)
	pending := streamInspect(t, last, lastLaunch)
	if pending.ID != expectedID {
		t.Fatal("attempted event was not replayed")
	}
	part := streamAttemptAll(t, last, lastLaunch, pending)
	var replayed streamFrame
	if json.Unmarshal(part[0], &replayed) != nil || replayed.Event == nil || *replayed.Event != expected {
		t.Fatal("event replay changed metadata")
	}
	acknowledgeStream(t, last, lastLaunch, pending.ID)
}

func assertPluginMetadataOnly(t *testing.T, d PluginDelivery, private ...string) {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range private {
		if bytes.Contains(raw, []byte(value)) {
			t.Fatal("metadata frame leaks", value)
		}
	}
}

func TestPluginFeedRejectsUnknownAndStaleAckIDs(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	updateStreamState(t, store, session.ID, model.StateBusy)
	delivery := streamInspect(t, feed, launch)
	prior := readCommitFile(t, feed.streamPath())
	if delivery.Event != nil || delivery.ID == "" {
		t.Fatal("prepared inspection exposed a body or lacked a receipt")
	}
	for _, id := range []string{model.NewUUID(), delivery.ID} {
		if _, err := feed.AckPluginFeed(launch, id); model.ErrorCodeOf(err) != model.CodeConflict {
			t.Fatal("F_ACK_UNKNOWN_OR_UNATTEMPTED_REFUSED", err)
		}
		if !bytes.Equal(prior, readCommitFile(t, feed.streamPath())) {
			t.Fatal("refused ACK changed checkpoint")
		}
	}
	streamAttemptAll(t, feed, launch, delivery)
	acknowledgeStream(t, feed, launch, delivery.ID)
	committed := readCommitFile(t, feed.streamPath())
	if pending, err := feed.AckPluginFeed(launch, delivery.ID); err != nil || pending != "" {
		t.Fatal("matching committed ACK was not an exact no-op", pending, err)
	}
	if !bytes.Equal(committed, readCommitFile(t, feed.streamPath())) {
		t.Fatal("F_ACK_COMMITTED_NOOP_EXACT_CHECKPOINT")
	}
	if next := streamInspect(t, feed, launch); next.ID != "" {
		t.Fatal("ACK did not advance", next)
	}
}

func TestPluginFeedExplicitGrantBounds(t *testing.T) {
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	status, err := NewPluginFeed(s, "fake-notifier").PluginStatus()
	if err != nil || status.Enabled || status.Scope != "" || !status.Access.empty() {
		t.Fatal("plugin is not disabled by default", status, err)
	}
	if _, err := os.Stat(NewPluginFeed(s, "fake-notifier").pluginPath()); !os.IsNotExist(err) {
		t.Fatal("status created configuration", err)
	}
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(true); model.ErrorCodeOf(err) != model.CodeNotFound {
		t.Fatal("enabled an absent installation", err)
	}
	installFeed(t, s)
	if err := NewPluginFeed(s, "fake-notifier").RevokePluginAccess(); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(true); err == nil {
		t.Fatal("enabled an installation without access")
	}
	tooMany := make([]string, 17)
	for i := range tooMany {
		tooMany[i] = "codex:" + model.NewUUID()
	}
	invalid := [][]string{{"*"}, {"codex:../escape"}, {strings.ToUpper(a.ID)}, {a.ID, a.ID}, tooMany}
	for _, ids := range invalid {
		if err := NewPluginFeed(s, "fake-notifier").GrantPluginAccess(PluginAccess{Read: ids}); err == nil {
			t.Fatal("accepted invalid named sessions", ids)
		}
	}
	if err := NewPluginFeed(s, "fake-notifier").GrantPluginAccess(PluginAccess{Read: tooMany[:16]}); err != nil {
		t.Fatal("16 valid named sessions rejected", err)
	}
}

func TestPluginFeedGrantChangesAndDisable(t *testing.T) {
	store := fixtureStore(t)
	a, _ := register(t, store, "codex")
	b, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, a.ID)
	primeStreamSession(t, store, feed, launch, b.ID)
	updateStreamState(t, store, a.ID, model.StateBusy)
	old := streamInspect(t, feed, launch)
	streamAttemptAll(t, feed, launch, old)
	before := readCommitFile(t, feed.streamPath())
	if err := feed.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if _, err := feed.AckPluginFeed(launch, old.ID); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("disabled captured receipt remained valid", err)
	}
	if _, err := feed.NextPluginFeed(t.Context(), launch, "", 0); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("disabled feed dispatched", err)
	}
	if !bytes.Equal(before, readCommitFile(t, feed.streamPath())) {
		t.Fatal("disabled failure changed checkpoint")
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if _, err := feed.AckPluginFeed(launch, old.ID); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("old captured launch crossed a new enable", err)
	}
	current := streamTestLaunch(t, feed)
	if _, err := feed.AckPluginFeed(current, old.ID); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("old receipt crossed current enable", err)
	}
	streamReady(t, feed, current)
	primeStreamSession(t, store, feed, current, a.ID)
	primeStreamSession(t, store, feed, current, b.ID)
	assertCurrentStreamStates(t, store, feed, current, []streamTestChange{{a.ID, model.StateReady}, {b.ID, model.StateBusy}})
	next, _, err := store.Register("codex", b.NativeID, b.CWD, nil)
	if err != nil {
		t.Fatal(err)
	}
	gap := streamInspect(t, feed, current)
	if gap.Gap != "policy" || gap.Event != nil {
		t.Fatal("replacement generation did not reset old interval", gap)
	}
	streamComplete(t, feed, current, gap)
	if cut := streamInspect(t, feed, current); cut.ID != "" {
		t.Fatal("B_REPLACEMENT_PRIVATE_CUT_HAS_NO_PUBLIC_ROWS")
	}
	primeStreamSession(t, store, feed, current, next.ID)
	updateStreamState(t, store, next.ID, model.StateBusy)
	offer, frame := streamTake(t, feed, current)
	if frame.Event == nil || frame.Event.Session.ID != b.ID || frame.Event.Session.Generation != next.Generation {
		t.Fatal("a member was lost across registration")
	}
	acknowledgeStream(t, feed, current, offer.ID)
}

type streamTestChange struct {
	id    string
	state model.SessionState
}

func assertCurrentStreamStates(t *testing.T, store bus.Store, feed PluginFeed, current *PluginFeedStatus, changes []streamTestChange) {
	t.Helper()
	for _, change := range changes {
		updateStreamState(t, store, change.id, change.state)
		event := streamInspect(t, feed, current)
		parts := streamAttemptAll(t, feed, current, event)
		var frame streamFrame
		if len(parts) != 1 || json.Unmarshal(parts[0], &frame) != nil || frame.Event == nil || frame.Event.Session.ID != change.id || frame.Event.Session.State != change.state {
			t.Fatal("scope-all member event lost", change.id)
		}
		acknowledgeStream(t, feed, current, event.ID)
	}
}

func pluginDirectorySyncCut(t *testing.T, s bus.Store, action func() error) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission cut requires an ordinary user")
	}
	t.Cleanup(func() { _ = os.Chmod(NewPluginFeed(s, "fake-notifier").pluginDir(), 0700) })
	if err := os.Chmod(NewPluginFeed(s, "fake-notifier").pluginDir(), 0300); err != nil {
		t.Fatal(err)
	}
	if err := action(); !errors.Is(err, os.ErrPermission) {
		t.Fatal("expected post-rename directory-sync failure", err)
	}
	feedRecord(t, s)
	if err := os.Chmod(NewPluginFeed(s, "fake-notifier").pluginDir(), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestPluginFeedGapDirectorySyncFailure(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	prior := readCommitFile(t, feed.streamPath())
	start := readStreamCheckpoint(t, feed).Cursor
	for range 3 {
		testCoveredUpdate(t, store, session.ID)
	}
	pluginDirectorySyncCut(t, store, func() error { _, err := feed.NextPluginFeed(t.Context(), launch, "", 0); return err })
	visible := readStreamCheckpoint(t, feed)
	if visible.Pending == nil || visible.Pending.Kind != "gap" || visible.LastGap != "retention" || visible.Cursor != start {
		t.Fatal("gap watermark was published without a pending notice", visible)
	}
	// Retaining the old namespace after a failed directory sync cannot hide loss.
	if err := fsutil.WriteAtomic(feed.streamPath(), prior); err != nil {
		t.Fatal(err)
	}
	gap := streamInspect(t, feed, launch)
	if gap.Gap != "retention" {
		t.Fatal("crash hid skipped history", gap)
	}
	streamComplete(t, feed, launch, gap)
}

func TestPluginFeedAckDirectorySyncFailure(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	primeStreamSession(t, store, feed, launch, session.ID)
	updateStreamState(t, store, session.ID, model.StateBusy)
	event := streamInspect(t, feed, launch)
	parts := streamAttemptAll(t, feed, launch, event)
	prior := readCommitFile(t, feed.streamPath())
	position := readStreamCheckpoint(t, feed).Pending.After
	pluginDirectorySyncCut(t, store, func() error { _, err := feed.AckPluginFeed(launch, event.ID); return err })
	current := readStreamCheckpoint(t, feed)
	if current.Cursor != position || current.LastAck != event.ID || current.Pending != nil {
		t.Fatal("cut did not reach the rename")
	}
	published := readCommitFile(t, feed.streamPath())
	if _, err := feed.AckPluginFeed(launch, event.ID); err != nil {
		t.Fatal("current-disk reconciliation failed", err)
	}
	if !bytes.Equal(published, readCommitFile(t, feed.streamPath())) {
		t.Fatal("current-disk reconciliation rolled back or changed evidence")
	}
	// Independently simulate power loss retaining the OLD checkpoint namespace.
	if err := fsutil.WriteAtomic(feed.streamPath(), prior); err != nil {
		t.Fatal(err)
	}
	restarted := NewPluginFeed(store, "fake-notifier")
	freezeStreamWork(restarted)
	replay := streamInspect(t, restarted, launch)
	if replay.ID != event.ID {
		t.Fatal("crash lost the unconfirmed position", replay)
	}
	replayParts := streamAttemptAll(t, restarted, launch, replay)
	if len(parts) != 1 || len(replayParts) != 1 || !bytes.Equal(parts[0], replayParts[0]) {
		t.Fatal("F_SYNC_OLD_NAMESPACE_EXACT_ATTEMPTED_REPLAY")
	}
	acknowledgeStream(t, restarted, launch, replay.ID)
}

func TestPluginFeedInvalidPrivateRecord(t *testing.T) {
	s := fixtureStore(t)
	register(t, s, "codex")
	enableFeed(t, s)
	good := readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())
	for _, raw := range [][]byte{
		[]byte(`{"enabled":true}`), []byte(`{"unknown":1}`),
		bytes.Repeat([]byte(" "), 16<<10+1),
		bytes.Replace(good, []byte(`"epoch": "`), []byte(`"epoch": "invalid-`), 1),
	} {
		if err := fsutil.WriteAtomic(NewPluginFeed(s, "fake-notifier").pluginPath(), raw); err != nil {
			t.Fatal(err)
		}
		if _, err := NewPluginFeed(s, "fake-notifier").PluginStatus(); err == nil {
			t.Fatal("invalid private record was accepted")
		}
		if _, err := nextFeed(NewPluginFeed(s, "fake-notifier")); err == nil {
			t.Fatal("invalid private record was dispatched")
		}
	}
}

func TestPluginFeedBoundedForeignScan(t *testing.T) {
	store := fixtureStore(t)
	register(t, store, "codex")
	foreign, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := fixtureFeedRun(NewPluginFeed(store, "fake-notifier"))
	launch := streamTestLaunch(t, feed)
	if d := streamInspect(t, feed, launch); d.ID != "" {
		t.Fatal("private cut offered a public fact")
	}
	start := readStreamCheckpoint(t, feed).Cursor.Sequence
	for i := range 130 {
		state := model.StateBusy
		if i%2 != 0 {
			state = model.StateReady
		}
		updateStreamState(t, store, foreign.ID, state)
	}
	forgetSession(t, store, foreign.ID)
	before := eventState(t, store)
	for _, offset := range []uint64{64, 128, 130} {
		d, err := feed.NextPluginFeed(t.Context(), launch, "", 0)
		if err != nil || d.ID != "" || d.Event != nil {
			t.Fatal("foreign metadata exposed", d, err)
		}
		if cursor := readStreamCheckpoint(t, feed).Cursor; cursor.Sequence != start+offset {
			t.Fatalf("unbounded scan: %+v", cursor)
		}
	}
	if after := eventState(t, store); after != before {
		t.Fatal("reader mutated event metadata")
	}
}

func TestPluginFeedRetentionGapIsDurableUntilACK(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	start := readStreamCheckpoint(t, feed).Cursor
	for range 3 {
		testCoveredUpdate(t, store, session.ID)
	}
	gap := streamInspect(t, feed, launch)
	if gap.Gap != "retention" || gap.Event != nil || !feedRecord(t, store).Enabled {
		t.Fatal("missing explicit loss control", gap)
	}
	first := streamAttemptAll(t, feed, launch, gap)
	watermark := readStreamCheckpoint(t, feed).Pending.After
	setEventTestState(t, store, session.ID, model.StateReady)
	again := streamInspect(t, feed, launch)
	second := streamAttemptAll(t, feed, launch, again)
	if again.ID != gap.ID || len(first) != 1 || len(second) != 1 || !bytes.Equal(first[0], second[0]) || readStreamCheckpoint(t, feed).Pending.After != watermark || readStreamCheckpoint(t, feed).Cursor != start {
		t.Fatal("pending plain gap not retained exactly", again)
	}
	acknowledgeStream(t, feed, launch, gap.ID)
	assertPlainRetentionRecovery(t, store, feed, launch, session.ID)
}

func assertPlainRetentionRecovery(t *testing.T, store bus.Store, feed PluginFeed, launch *PluginFeedStatus, id string) {
	t.Helper()
	cut := streamInspect(t, feed, launch)
	if cut.ID != "" || cut.Event != nil || len(cut.Frames) != 0 {
		t.Fatal("F_PLAIN_RETENTION_CUT_HAS_NO_UNNEGOTIATED_ROWS", cut)
	}
	if readStreamCheckpoint(t, feed).Cursor.Sequence != eventState(t, store).Last {
		t.Fatal("plain cut did not adopt current head")
	}
	updateStreamState(t, store, id, model.StateBusy)
	if d := streamInspect(t, feed, launch); d.ID != "" {
		t.Fatal("plain first unseen interval escaped fresh floor")
	}
	updateStreamState(t, store, id, model.StateReady)
	delivery, frame := streamTake(t, feed, launch)
	attempt, err := feed.NextPluginFeed(t.Context(), launch, delivery.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	assertNoBulkStreamFields(t, attempt.Frames[0])
	if frame.Event == nil || frame.Event.Session.ID != id || frame.Event.Session.State != model.StateReady {
		t.Fatal("F_PLAIN_RETENTION_POST_CUT_EVENT_POSITIVE")
	}
	acknowledgeStream(t, feed, launch, delivery.ID)
}

func TestPluginFeedEmptyEpochAndSequenceLimit(t *testing.T) {
	store := fixtureStore(t)
	enableFeed(t, store)
	feed := fixtureFeedRun(NewPluginFeed(store, "fake-notifier"))
	launch := streamTestLaunch(t, feed)
	if d := streamInspect(t, feed, launch); d.ID != "" {
		t.Fatal("empty journal generated a new gap", d)
	}
	session, _, err := store.Register("codex", model.NewUUID(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first := streamInspect(t, feed, launch); first.ID != "" {
		t.Fatal("unobserved first interval escaped its private floor", first)
	}
	updateStreamState(t, store, session.ID, model.StateBusy)
	delivery := streamInspect(t, feed, launch)
	streamComplete(t, feed, launch, delivery)
	old := readStreamCheckpoint(t, feed)
	head := eventState(t, store)
	head.Epoch = model.NewUUID()
	head.First, head.Last, head.Count = math.MaxUint64, math.MaxUint64, 1
	if err := writeJSON(filepath.Join(store.Dir(), eventMetaPath), head); err != nil {
		t.Fatal(err)
	}
	gap := streamInspect(t, feed, launch)
	if gap.Gap != "epoch" || gap.Event != nil || gap.ID == old.LastAck {
		t.Fatal("epoch change was not explicit", gap)
	}
	streamComplete(t, feed, launch, gap)
	if d := streamInspect(t, feed, launch); d.ID != "" {
		t.Fatal("max sequence wrapped", d)
	}
	if cursor := readStreamCheckpoint(t, feed).Cursor; cursor.Sequence != math.MaxUint64 || cursor.Epoch != head.Epoch {
		t.Fatal("max sequence checkpoint changed", cursor)
	}
}

func TestPluginFeedOwnerSymlinkRemainsClosed(t *testing.T) {
	store := fixtureStore(t)
	register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	original := readCommitFile(t, feed.pluginPath())
	target := filepath.Join(t.TempDir(), "owner.json")
	if err := fsutil.WriteAtomic(target, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(feed.pluginPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, feed.pluginPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := feed.PluginStatus(); !errors.Is(err, journal.ErrInvalidFile) {
		t.Fatal("plugin record followed a symlink", err)
	}
	if !bytes.Equal(original, readCommitFile(t, target)) {
		t.Fatal("invalid owner read rewrote target")
	}
}

func TestPluginFeedPendingGapSurvivesLaterLoss(t *testing.T) {
	for _, reason := range []string{"retention", "epoch"} {
		t.Run(reason, func(t *testing.T) {
			checkPluginPendingGapAfterLoss(t, reason)
		})
	}
}

func checkPluginPendingGapAfterLoss(t *testing.T, reason string) {
	t.Helper()
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := fixtureFeedRun(NewPluginFeed(store, "fake-notifier"))
	launch := streamTestLaunch(t, feed)
	streamInspect(t, feed, launch)
	for range 3 {
		testCoveredUpdate(t, store, session.ID)
	}
	pending := streamInspect(t, feed, launch)
	parts := streamAttemptAll(t, feed, launch, pending)
	if pending.Gap != "retention" {
		t.Fatal("missing first retention gap", pending)
	}
	watermark := readStreamCheckpoint(t, feed).Pending.After
	if reason == "retention" {
		for range 3 {
			testCoveredUpdate(t, store, session.ID)
		}
	} else {
		head := eventState(t, store)
		head.Epoch = model.NewUUID()
		if err := writeJSON(filepath.Join(store.Dir(), eventMetaPath), head); err != nil {
			t.Fatal(err)
		}
	}
	// Another Store value shares the same continuing host, not a fresh child.
	reopened := fixtureFeedRun(NewPluginFeed(bus.StoreAt(store.Dir(), bus.WithRegistry(store.Registry())), "fake-notifier"))
	again := streamInspect(t, reopened, launch)
	againParts := streamAttemptAll(t, reopened, launch, again)
	if again.ID != pending.ID || again.Gap != pending.Gap || readStreamCheckpoint(t, reopened).Pending.After != watermark || len(parts) != 1 || len(againParts) != 1 || !bytes.Equal(parts[0], againParts[0]) {
		t.Fatal("pending gap was replaced before ACK", pending, again)
	}
	acknowledgeStream(t, reopened, launch, pending.ID)
	next := streamInspect(t, reopened, launch)
	if next.Gap != reason || next.ID == pending.ID {
		t.Fatal("later loss was not reported after the original ACK", next)
	}
}

func TestPluginFeedReceiptAfterFilteredProgress(t *testing.T) {
	store := fixtureStore(t)
	a, _ := register(t, store, "codex")
	b, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	// Establish A's private floor before the actual acknowledged fact.
	updateStreamState(t, store, a.ID, model.StateBusy)
	if d := streamInspect(t, feed, launch); d.ID != "" {
		t.Fatal("first unseen interval escaped")
	}
	updateStreamState(t, store, a.ID, model.StateReady)
	old := streamInspect(t, feed, launch)
	streamComplete(t, feed, launch, old)
	updateStreamState(t, store, b.ID, model.StateBusy)
	forgetSession(t, store, b.ID)
	if d := streamInspect(t, feed, launch); d.ID != "" {
		t.Fatal("foreign event was offered", d)
	}
	prior := readCommitFile(t, feed.streamPath())
	updateStreamState(t, store, a.ID, model.StateBusy)
	if pending, err := feed.AckPluginFeed(launch, old.ID); err != nil || pending != "" {
		t.Fatal("matching committed receipt was not an exact no-op", pending, err)
	}
	if !bytes.Equal(prior, readCommitFile(t, feed.streamPath())) {
		t.Fatal("repeated committed ACK changed filtered cursor/table/evidence")
	}
	delivery := streamInspect(t, feed, launch)
	parts := streamAttemptAll(t, feed, launch, delivery)
	var frame streamFrame
	if len(parts) != 1 || json.Unmarshal(parts[0], &frame) != nil || frame.Event == nil || frame.Event.Session.ID != a.ID || frame.Event.Session.State != model.StateBusy {
		t.Fatal("later granted event was lost", delivery)
	}
	if replay := streamInspect(t, feed, launch); replay.ID != delivery.ID {
		t.Fatal("unacknowledged granted event did not replay", replay)
	}
	acknowledgeStream(t, feed, launch, delivery.ID)
}

func TestPluginFeedEmptyEnableFirstEventReplay(t *testing.T) {
	store := fixtureStore(t)
	installFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	launch := streamTestLaunch(t, feed)
	gap := streamInspect(t, feed, launch)
	session, _, err := store.Register("codex", model.NewUUID(), t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if again := streamInspect(t, feed, launch); again.ID != gap.ID {
		t.Fatal("first real epoch replaced the enable notice", again)
	}
	streamComplete(t, feed, launch, gap)
	if d := streamInspect(t, feed, launch); d.ID != "" {
		t.Fatal("fresh private cut exposed pre-cut registration", d)
	}
	updateStreamState(t, store, session.ID, model.StateBusy)
	if d := streamInspect(t, feed, launch); d.ID != "" {
		t.Fatal("first unseen native interval escaped its floor", d)
	}
	updateStreamState(t, store, session.ID, model.StateReady)
	delivery := streamInspect(t, feed, launch)
	parts := streamAttemptAll(t, feed, launch, delivery)
	restarted := NewPluginFeed(store, "fake-notifier")
	freezeStreamWork(restarted)
	replay := streamInspect(t, restarted, launch)
	if replay.ID != delivery.ID {
		t.Fatal("first safe attempted event was not replayed before ACK", replay)
	}
	replayParts := streamAttemptAll(t, restarted, launch, replay)
	if len(parts) != 1 || len(replayParts) != 1 || !bytes.Equal(parts[0], replayParts[0]) {
		t.Fatal("first safe attempted event changed on replay")
	}
	acknowledgeStream(t, restarted, launch, replay.ID)
	// A restart's required fresh cut is a distinct gap, never duplicate event data.
	next := streamInspect(t, restarted, launch)
	if next.Gap != "launch" || next.Event != nil || next.ID == delivery.ID {
		t.Fatal("first event duplicated after ACK", next)
	}
}

func TestPluginFeedInvalidGrantsAndPosition(t *testing.T) {
	cases := map[string]func(*pluginRecord){
		"unsorted":       func(p *pluginRecord) { p.Access.Read = []string{p.Access.Read[1], p.Access.Read[0]} },
		"duplicate":      func(p *pluginRecord) { p.Access.Read[1] = p.Access.Read[0] },
		"bad_epoch":      func(p *pluginRecord) { p.Epoch = "invalid" },
		"empty_grants":   func(p *pluginRecord) { p.Access = PluginAccess{} },
		"stale_revision": func(p *pluginRecord) { p.Scope = "none" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) { checkHandEditedRecord(t, change, name != "stale_revision") })
	}
}

// checkHandEditedRecord edits a valid enabled record and checks that it is
// refused, not dispatched and not changed. With rehash, only the edited field
// is wrong; without, the stored revision is stale too.
func checkHandEditedRecord(t *testing.T, change func(*pluginRecord), rehash bool) {
	t.Helper()
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	b, _ := register(t, s, "codex")
	enableFeed(t, s)
	f := NewPluginFeed(s, "fake-notifier")
	read := []string{a.ID, b.ID}
	slices.Sort(read)
	for _, step := range []func() error{func() error { return f.EnablePluginFeed(false) }, func() error { return f.GrantPluginAccess(PluginAccess{Status: true, Read: read}) }, func() error { return f.EnablePluginFeed(true) }} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	p := feedRecord(t, s)
	change(&p)
	if rehash {
		p.Revision = pluginRevision(p)
	}
	raw, err := journal.EncodeJSON(p, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteAtomic(f.pluginPath(), raw); err != nil {
		t.Fatal(err)
	}
	if _, err := f.PluginStatus(); err == nil {
		t.Fatal("hand-edited record was accepted")
	}
	if _, err := nextFeed(f); err == nil {
		t.Fatal("hand-edited record was dispatched")
	}
	if !bytes.Equal(raw, readCommitFile(t, f.pluginPath())) {
		t.Fatal("refusing a record changed it")
	}
}

func TestPluginFeedRejectsZeroFirstMetadata(t *testing.T) {
	s := fixtureStore(t)
	register(t, s, "codex")
	enableFeed(t, s)
	prior := readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())
	head := eventState(t, s)
	head.First = 0
	if err := writeJSON(filepath.Join(s.Dir(), eventMetaPath), head); err != nil {
		t.Fatal(err)
	}
	if _, err := nextFeed(NewPluginFeed(s, "fake-notifier")); !errors.Is(err, journal.ErrInvalidBatch) {
		t.Fatal("nonempty zero-first metadata reached loss arithmetic", err)
	}
	if !bytes.Equal(prior, readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())) {
		t.Fatal("invalid metadata changed the plugin position")
	}
}

func denyPluginDirectoryRead(t *testing.T, s bus.Store) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permission cut requires an ordinary user")
	}
	t.Cleanup(func() { _ = os.Chmod(NewPluginFeed(s, "fake-notifier").pluginDir(), 0700) })
	if err := os.Chmod(NewPluginFeed(s, "fake-notifier").pluginDir(), 0300); err != nil {
		t.Fatal(err)
	}
}

func TestPluginFeedPendingGapRequiresDirectoryConfirmation(t *testing.T) {
	store := fixtureStore(t)
	session, _ := register(t, store, "codex")
	enableFeed(t, store)
	feed := NewPluginFeed(store, "fake-notifier")
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	for range 3 {
		testCoveredUpdate(t, store, session.ID)
	}
	denyPluginDirectoryRead(t, store)
	if d, err := feed.NextPluginFeed(t.Context(), launch, "", 0); !errors.Is(err, os.ErrPermission) || d.ID != "" {
		t.Fatal("expected post-rename gap sync failure with no bytes", d, err)
	}
	if err := os.Chmod(feed.pluginDir(), 0700); err != nil {
		t.Fatal(err)
	}
	visible := readStreamCheckpoint(t, feed)
	if visible.Pending == nil || visible.LastGap != "retention" {
		t.Fatal("cut did not publish the pending gap")
	}
	prior := readCommitFile(t, feed.streamPath())
	id := visible.Pending.ID
	denyPluginDirectoryRead(t, store)
	if d, err := feed.NextPluginFeed(t.Context(), launch, id, 1); !errors.Is(err, os.ErrPermission) || d.ID != "" {
		t.Fatal("unconfirmed pending gap was offered after failed directory sync", d, err)
	}
	if err := os.Chmod(feed.pluginDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prior, readCommitFile(t, feed.streamPath())) {
		t.Fatal("F_CHECKPOINT_CONFIRMATION_FAILURE_NO_ATTEMPT_MUTATION: failed durability confirmation changed the record")
	}
	gap := streamInspect(t, feed, launch)
	if gap.ID != id {
		t.Fatal("durability confirmation replaced the pending gap", gap)
	}
	streamComplete(t, feed, launch, gap)
}

func TestPluginFeedEnableRetryConfirmsDirectory(t *testing.T) {
	for name, enabled := range map[string]bool{"enable": true, "disable": false} {
		t.Run(name, func(t *testing.T) { checkPluginEnableRetry(t, enabled) })
	}
}

func checkPluginEnableRetry(t *testing.T, enabled bool) {
	t.Helper()
	s := fixtureStore(t)
	installFeed(t, s)
	if err := NewPluginFeed(s, "fake-notifier").ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").GrantPluginAccess(PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if !enabled {
		if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(true); err != nil {
			t.Fatal(err)
		}
	}
	denyPluginDirectoryRead(t, s)
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(enabled); !errors.Is(err, os.ErrPermission) {
		t.Fatal("expected post-rename enable/disable sync failure", err)
	}
	if p := feedRecord(t, s); p.Enabled != enabled {
		t.Fatal("cut did not reach replacement", p)
	}
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(enabled); !errors.Is(err, os.ErrPermission) {
		t.Fatal("idempotent retry reported success without directory confirmation", err)
	}
	if err := os.Chmod(NewPluginFeed(s, "fake-notifier").pluginDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(enabled); err != nil {
		t.Fatal(err)
	}
	if p := feedRecord(t, s); p.Enabled != enabled {
		t.Fatal("confirmed enable/disable state differs", p)
	}
}

// forgetSession removes a session record, so the session is no member while
// its earlier events stay in the journal.
func forgetSession(t *testing.T, s bus.Store, id string) {
	t.Helper()
	if err := os.Remove(filepath.Join(s.Dir(), "sessions", strings.ReplaceAll(id, ":", "_")+".json")); err != nil {
		t.Fatal(err)
	}
}

// expectSessionEvents takes one offer per session, in order, and acks it.

func assertNoBulkStreamFields(t *testing.T, raw []byte) {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"members", "members_end", "member"} {
		if _, present := object[key]; present {
			t.Fatal("F_PLAIN_RETENTION_POST_CUT_EVENT_POSITIVE")
		}
	}
}
