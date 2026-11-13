package pluginfeed

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
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
	}
}

func enableFeed(t *testing.T, s bus.Store, ids ...string) {
	t.Helper()
	installFeed(t, s)
	if err := NewPluginFeed(s, "fake-notifier").ConfigurePluginFeed(ids); err != nil {
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
	if err := NewPluginFeed(s, "fake-notifier").AckPluginFeed(d); err != nil {
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
	s := fixtureStore(t)
	v, key := register(t, s, "codex")
	if _, err := (&bus.Service{Store: s}).SetTask(v.ID, key, "PRIVATE_TASK"); err != nil {
		t.Fatal(err)
	}
	installFeed(t, s)
	if err := NewPluginFeed(s, "fake-notifier").ConfigurePluginFeed([]string{v.ID}); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	gap := feedNext(t, s)
	if gap.Gap != "enabled" || gap.Event != nil || gap.ID == "" {
		t.Fatal("expected explicit skipped-history notice", gap)
	}
	if err := s.Update(v.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	reopened := bus.Store{Dir: s.Dir, Registry: s.Registry}
	if again := feedNext(t, reopened); again.ID != gap.ID {
		t.Fatal("unacknowledged gap disappeared across restart")
	}
	feedAck(t, reopened, gap)
	d := feedNext(t, reopened)
	if d.Event == nil || d.Event.Session.State != model.StateBusy || d.Gap != "" {
		t.Fatal("event after enable watermark was skipped", d)
	}
	assertPluginMetadataOnly(t, d, "PRIVATE_TASK", key, v.CWD, "epoch", "sequence", "position", "prior", "adapter_state")
	if next := feedNext(t, reopened); next.ID != d.ID {
		t.Fatal("unacknowledged event was not replayed")
	}
	feedAck(t, s, d)
	assertPluginGapAcknowledged(t, s)
}

func assertPluginGapAcknowledged(t *testing.T, s bus.Store) {
	t.Helper()
	status, err := NewPluginFeed(s, "fake-notifier").PluginStatus()
	if err != nil || status.GapPending || status.LastGap != "enabled" {
		t.Fatalf("gap status = %+v, %v", status, err)
	}
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

func TestPluginFeedRejectsForgedAndStaleReceipts(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	if err := s.Update(v.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	d := feedNext(t, s)
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	var forged PluginDelivery
	if err := json.Unmarshal(raw, &forged); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").AckPluginFeed(forged); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("child frame forged a host receipt", err)
	}
	changed := d
	changed.ID = model.NewUUID()
	if err := NewPluginFeed(s, "fake-notifier").AckPluginFeed(changed); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("ACK accepted another delivery ID", err)
	}
	feedAck(t, s, d)
	if err := NewPluginFeed(s, "fake-notifier").AckPluginFeed(d); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("reused receipt advanced position twice", err)
	}
	if next := feedNext(t, s); next.ID != "" {
		t.Fatal("ACK did not advance", next)
	}
}

func TestPluginFeedExplicitGrantBounds(t *testing.T) {
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	status, err := NewPluginFeed(s, "fake-notifier").PluginStatus()
	if err != nil || status.Enabled || len(status.Sessions) != 0 {
		t.Fatal("plugin is not disabled by default", status, err)
	}
	if _, err := os.Stat(NewPluginFeed(s, "fake-notifier").pluginPath()); !os.IsNotExist(err) {
		t.Fatal("status created configuration", err)
	}
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(true); model.ErrorCodeOf(err) != model.CodeNotFound {
		t.Fatal("enabled an absent installation", err)
	}
	installFeed(t, s)
	tooMany := make([]string, 17)
	for i := range tooMany {
		tooMany[i] = "codex:" + model.NewUUID()
	}
	invalid := [][]string{{"*"}, {"codex:../escape"}, {strings.ToUpper(a.ID)}, {a.ID, a.ID}, tooMany}
	for _, ids := range invalid {
		if err := NewPluginFeed(s, "fake-notifier").ConfigurePluginFeed(ids); err == nil {
			t.Fatal("accepted invalid scope", ids)
		}
	}
	if err := NewPluginFeed(s, "fake-notifier").ConfigurePluginFeed(tooMany[:16]); err != nil {
		t.Fatal("16 valid grants rejected", err)
	}
}

func TestPluginFeedGrantChangesAndDisable(t *testing.T) {
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	b, _ := register(t, s, "codex")
	enableFeed(t, s, a.ID)
	if err := NewPluginFeed(s, "fake-notifier").ConfigurePluginFeed([]string{b.ID}); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("scope changed during execution", err)
	}
	if err := s.Update(a.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	old := feedNext(t, s)
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").AckPluginFeed(old); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("disabled receipt remained valid", err)
	}
	if _, err := nextFeed(NewPluginFeed(s, "fake-notifier")); model.ErrorCodeOf(err) != model.CodeUnauthorized {
		t.Fatal("disabled feed dispatched", err)
	}
	enableFeed(t, s, b.ID)
	if err := NewPluginFeed(s, "fake-notifier").AckPluginFeed(old); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("old receipt crossed a new enable/scope", err)
	}
	if err := s.Update(a.ID, model.StateReady, false); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(b.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	d := feedNext(t, s)
	if d.Event == nil || d.Event.Session.ID != b.ID {
		t.Fatal("foreign session escaped filtering", d)
	}
	feedAck(t, s, d)
	next, _, err := s.Register("codex", b.NativeID, b.CWD, nil)
	if err != nil {
		t.Fatal(err)
	}
	d = feedNext(t, s)
	if d.Event == nil || d.Event.Type != "session.registered" || d.Event.Session.Generation != next.Generation {
		t.Fatal("explicit session grant failed across registration", d)
	}
}

func pluginDirectorySyncCut(t *testing.T, s bus.Store, action func() error) pluginRecord {
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
	live := feedRecord(t, s)
	if err := os.Chmod(NewPluginFeed(s, "fake-notifier").pluginDir(), 0700); err != nil {
		t.Fatal(err)
	}
	return live
}

func TestPluginFeedGapDirectorySyncFailure(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	prior := readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())
	for range 3 {
		testCoveredUpdate(t, s, v.ID)
	}
	live := pluginDirectorySyncCut(t, s, func() error { _, err := nextFeed(NewPluginFeed(s, "fake-notifier")); return err })
	if live.GapID == "" || live.GapReason != "retention" {
		t.Fatal("gap watermark was published without a pending notice", live)
	}
	// Simulate power loss retaining the old namespace after failed sync.
	if err := journal.WriteAtomic(NewPluginFeed(s, "fake-notifier").pluginPath(), prior); err != nil {
		t.Fatal(err)
	}
	d := feedNext(t, s)
	if d.Gap != "retention" {
		t.Fatal("crash hid skipped history", d)
	}
	feedAck(t, s, d)
}

func TestPluginFeedAckDirectorySyncFailure(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	if err := s.Update(v.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	d := feedNext(t, s)
	prior := readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())
	live := pluginDirectorySyncCut(t, s, func() error { return NewPluginFeed(s, "fake-notifier").AckPluginFeed(d) })
	if live.Position != d.after {
		t.Fatal("cut did not reach the rename")
	}
	if err := journal.WriteAtomic(NewPluginFeed(s, "fake-notifier").pluginPath(), prior); err != nil {
		t.Fatal(err)
	}
	replayed := feedNext(t, s)
	if replayed.ID != d.ID {
		t.Fatal("crash lost the unconfirmed position", replayed)
	}
	feedAck(t, s, replayed)
}

func TestPluginFeedInvalidPrivateRecord(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	good := readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())
	for _, raw := range [][]byte{
		[]byte(`{"enabled":true}`), []byte(`{"unknown":1}`),
		bytes.Repeat([]byte(" "), 16<<10+1),
		bytes.Replace(good, []byte(`"gap_reason": "enabled"`), []byte(`"gap_reason": "hidden"`), 1),
	} {
		if err := journal.WriteAtomic(NewPluginFeed(s, "fake-notifier").pluginPath(), raw); err != nil {
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
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	b, _ := register(t, s, "codex")
	enableFeed(t, s, a.ID)
	start := feedRecord(t, s).Position.Sequence
	for i := range 130 {
		state := model.StateBusy
		if i%2 != 0 {
			state = model.StateReady
		}
		if err := s.Update(b.ID, state, false); err != nil {
			t.Fatal(err)
		}
	}
	before := eventState(t, s)
	for _, offset := range []uint64{64, 128, 130} {
		if d := feedNext(t, s); d.ID != "" || d.Event != nil {
			t.Fatal("foreign metadata exposed", d)
		}
		if p := feedRecord(t, s); p.Position.Sequence != start+offset {
			t.Fatalf("unbounded scan: %+v", p.Position)
		}
	}
	if after := eventState(t, s); after != before {
		t.Fatal("reader mutated event metadata")
	}
}

func TestPluginFeedRetentionGapIsDurableUntilACK(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	for range 3 {
		testCoveredUpdate(t, s, v.ID)
	}
	d := feedNext(t, s)
	if d.Gap != "retention" || d.Event != nil || !feedRecord(t, s).Enabled {
		t.Fatal("missing explicit loss control", d)
	}
	watermark := feedRecord(t, s).Position.Sequence
	setEventTestState(t, s, v.ID, model.StateReady)
	if again := feedNext(t, s); again.ID != d.ID {
		t.Fatal("pending gap not retained", again)
	}
	feedAck(t, s, d)
	next := feedNext(t, s)
	if next.Event == nil || next.after.Sequence != watermark+1 {
		t.Fatal("gap ACK lost a later live event", next)
	}
}

func TestPluginFeedEmptyEpochAndSequenceLimit(t *testing.T) {
	s := fixtureStore(t)
	id := model.NewUUID()
	enableFeed(t, s, "codex:"+id)
	if d := feedNext(t, s); d.ID != "" {
		t.Fatal("empty journal generated a new gap", d)
	}
	if _, _, err := s.Register("codex", id, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	d := feedNext(t, s)
	if d.Event == nil || d.Event.Type != "session.registered" {
		t.Fatal("first real epoch lost its first event", d)
	}
	feedAck(t, s, d)
	p := feedRecord(t, s)
	head := eventState(t, s)
	head.Epoch = model.NewUUID()
	head.First, head.Last, head.Count = math.MaxUint64, math.MaxUint64, 1
	if err := writeJSON(filepath.Join(s.Dir, eventMetaPath), head); err != nil {
		t.Fatal(err)
	}
	d = feedNext(t, s)
	if d.Gap != "epoch" || d.Event != nil || d.ID == p.GapID {
		t.Fatal("epoch change was not explicit", d)
	}
	feedAck(t, s, d)
	if d := feedNext(t, s); d.ID != "" {
		t.Fatal("max sequence wrapped", d)
	}
}

func TestPluginFeedRejectsBrokenRecordsWithoutAdvancing(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	if err := s.Update(v.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	p := feedRecord(t, s)
	path := filepath.Join(s.Dir, eventRecordPath(p.Position.Sequence+1))
	good := readCommitFile(t, path)
	for _, broken := range [][]byte{[]byte(`{}`), bytes.Repeat([]byte(" "), maxEventBytes+1), bytes.Replace(good, []byte(p.Position.Epoch), []byte(model.NewUUID()), 1)} {
		if err := journal.WriteAtomic(path, broken); err != nil {
			t.Fatal(err)
		}
		if _, err := nextFeed(NewPluginFeed(s, "fake-notifier")); err == nil {
			t.Fatal("accepted corrupt source event")
		}
		if next := feedRecord(t, s); next.Position != p.Position {
			t.Fatal("error advanced the position")
		}
	}
	if err := journal.WriteAtomic(path, good); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(NewPluginFeed(s, "fake-notifier").pluginPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(path, NewPluginFeed(s, "fake-notifier").pluginPath()); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPluginFeed(s, "fake-notifier").PluginStatus(); !errors.Is(err, journal.ErrInvalidFile) {
		t.Fatal("plugin record followed a symlink", err)
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
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	for range 3 {
		testCoveredUpdate(t, s, v.ID)
	}
	pending := feedNext(t, s)
	if pending.Gap != "retention" {
		t.Fatal("missing first retention gap", pending)
	}
	if reason == "retention" {
		for range 3 {
			testCoveredUpdate(t, s, v.ID)
		}
	} else {
		head := eventState(t, s)
		head.Epoch = model.NewUUID()
		if err := writeJSON(filepath.Join(s.Dir, eventMetaPath), head); err != nil {
			t.Fatal(err)
		}
	}
	again := feedNext(t, bus.Store{Dir: s.Dir, Registry: s.Registry})
	if again.ID != pending.ID || again.Gap != pending.Gap || again.after != pending.after {
		t.Fatal("pending gap was replaced before ACK", pending, again)
	}
	feedAck(t, s, pending)
	next := feedNext(t, s)
	if next.Gap != reason || next.ID == pending.ID {
		t.Fatal("later loss was not reported after the original ACK", next)
	}
}

func TestPluginFeedReceiptAfterFilteredProgress(t *testing.T) {
	s := fixtureStore(t)
	a, _ := register(t, s, "codex")
	b, _ := register(t, s, "codex")
	enableFeed(t, s, a.ID)
	if err := s.Update(a.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	old := feedNext(t, s)
	other := bus.Store{Dir: s.Dir, Registry: s.Registry}
	feedAck(t, other, old)
	if err := s.Update(b.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	if d := feedNext(t, other); d.ID != "" {
		t.Fatal("foreign event was offered", d)
	}
	prior := readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())
	if err := s.Update(a.ID, model.StateReady, false); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").AckPluginFeed(old); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("stale receipt crossed a filtered-progress save", err)
	}
	if !bytes.Equal(prior, readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())) {
		t.Fatal("refused stale ACK changed the durable position")
	}
	d := feedNext(t, s)
	if d.Event == nil || d.Event.Session.ID != a.ID || d.Event.Session.State != model.StateReady {
		t.Fatal("later granted event was lost", d)
	}
	if replay := feedNext(t, other); replay.ID != d.ID {
		t.Fatal("unacknowledged granted event did not replay", replay)
	}
	feedAck(t, s, d)
}

func TestPluginFeedEmptyEnableFirstEventReplay(t *testing.T) {
	s := fixtureStore(t)
	id := model.NewUUID()
	installFeed(t, s)
	if err := NewPluginFeed(s, "fake-notifier").ConfigurePluginFeed([]string{"codex:" + id}); err != nil {
		t.Fatal(err)
	}
	if err := NewPluginFeed(s, "fake-notifier").EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	gap := feedNext(t, s)
	if _, _, err := s.Register("codex", id, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	if again := feedNext(t, s); again.ID != gap.ID {
		t.Fatal("first real epoch replaced the enable notice", again)
	}
	feedAck(t, s, gap)
	d := feedNext(t, s)
	if d.Event == nil || d.Event.Type != "session.registered" || d.after.Sequence != 1 {
		t.Fatal("first event was skipped", d)
	}
	if again := feedNext(t, s); again.ID != d.ID {
		t.Fatal("first event was not replayed before ACK", again)
	}
	feedAck(t, s, d)
	if next := feedNext(t, s); next.ID != "" {
		t.Fatal("first event was duplicated after ACK", next)
	}
}

func TestPluginFeedInvalidGrantsAndPosition(t *testing.T) {
	cases := map[string]func(*pluginRecord){
		"unsorted":               func(p *pluginRecord) { p.Sessions[0], p.Sessions[1] = p.Sessions[1], p.Sessions[0] },
		"duplicate":              func(p *pluginRecord) { p.Sessions[1] = p.Sessions[0] },
		"bad_epoch":              func(p *pluginRecord) { p.Position.Epoch = "invalid" },
		"empty_grants":           func(p *pluginRecord) { p.Sessions = nil },
		"sequence_without_epoch": func(p *pluginRecord) { p.Position = pluginPosition{Sequence: 1} },
		"bad_gap_id":             func(p *pluginRecord) { p.GapID = "invalid" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := fixtureStore(t)
			a, _ := register(t, s, "codex")
			b, _ := register(t, s, "codex")
			enableFeed(t, s, a.ID, b.ID)
			p := feedRecord(t, s)
			change(&p)
			raw, err := journal.EncodeJSON(p, 16<<10)
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.WriteAtomic(NewPluginFeed(s, "fake-notifier").pluginPath(), raw); err != nil {
				t.Fatal(err)
			}
			if _, err := NewPluginFeed(s, "fake-notifier").PluginStatus(); err == nil {
				t.Fatal("hand-edited record was accepted")
			}
			if _, err := nextFeed(NewPluginFeed(s, "fake-notifier")); err == nil {
				t.Fatal("hand-edited record was dispatched")
			}
			if !bytes.Equal(raw, readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())) {
				t.Fatal("refusing a record changed it")
			}
		})
	}
}

func TestPluginFeedRejectsZeroFirstMetadata(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	prior := readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())
	head := eventState(t, s)
	head.First = 0
	if err := writeJSON(filepath.Join(s.Dir, eventMetaPath), head); err != nil {
		t.Fatal(err)
	}
	if _, err := nextFeed(NewPluginFeed(s, "fake-notifier")); !errors.Is(err, journal.ErrInvalidBatch) {
		t.Fatal("nonempty zero-first metadata reached loss arithmetic", err)
	}
	if !bytes.Equal(prior, readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())) {
		t.Fatal("invalid metadata changed the plugin position")
	}
}

func TestPluginFeedBoundsValidOversizeEvent(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	if err := s.Update(v.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	prior := readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())
	path := filepath.Join(s.Dir, eventRecordPath(feedRecord(t, s).Position.Sequence+1))
	good := readCommitFile(t, path)
	large := bytes.Repeat([]byte(" "), maxEventBytes+1)
	copy(large, good)
	if !json.Valid(large) {
		t.Fatal("oversize fixture is not otherwise valid JSON")
	}
	if err := journal.WriteAtomic(path, large); err != nil {
		t.Fatal(err)
	}
	if _, err := nextFeed(NewPluginFeed(s, "fake-notifier")); !errors.Is(err, journal.ErrInvalidFile) {
		t.Fatal("valid oversized event was not refused by the bounded reader", err)
	}
	if !bytes.Equal(prior, readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())) {
		t.Fatal("oversize event advanced the plugin position")
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
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	enableFeed(t, s, v.ID)
	for range 3 {
		testCoveredUpdate(t, s, v.ID)
	}
	denyPluginDirectoryRead(t, s)
	if _, err := nextFeed(NewPluginFeed(s, "fake-notifier")); !errors.Is(err, os.ErrPermission) {
		t.Fatal("expected post-rename gap sync failure", err)
	}
	visible := feedRecord(t, s)
	if visible.GapID == "" || visible.GapReason != "retention" {
		t.Fatal("cut did not publish the pending gap")
	}
	prior := readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())
	if d, err := nextFeed(NewPluginFeed(s, "fake-notifier")); !errors.Is(err, os.ErrPermission) || d.ID != "" {
		t.Fatal("unconfirmed pending gap was offered after failed directory sync", d, err)
	}
	if !bytes.Equal(prior, readCommitFile(t, NewPluginFeed(s, "fake-notifier").pluginPath())) {
		t.Fatal("failed durability confirmation changed the record")
	}
	if err := os.Chmod(NewPluginFeed(s, "fake-notifier").pluginDir(), 0700); err != nil {
		t.Fatal(err)
	}
	d := feedNext(t, s)
	if d.ID != visible.GapID {
		t.Fatal("durability confirmation replaced the pending gap", d)
	}
	feedAck(t, s, d)
}

func TestPluginFeedEnableRetryConfirmsDirectory(t *testing.T) {
	for name, enabled := range map[string]bool{"enable": true, "disable": false} {
		t.Run(name, func(t *testing.T) { checkPluginEnableRetry(t, enabled) })
	}
}

func checkPluginEnableRetry(t *testing.T, enabled bool) {
	t.Helper()
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	installFeed(t, s)
	if err := NewPluginFeed(s, "fake-notifier").ConfigurePluginFeed([]string{v.ID}); err != nil {
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
