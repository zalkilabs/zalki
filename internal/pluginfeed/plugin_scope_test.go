package pluginfeed

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// inView runs fn with a journal view of the store.
func inView(t *testing.T, s bus.Store, fn func(bus.JournalView)) {
	t.Helper()
	if err := bus.WithJournalRead(s, func(view bus.JournalView) error {
		fn(view)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Only "all" and "none" can be written, and only the exact "all" gives
// standard access: a hand-written or unknown value is none.
func TestScopeIsAllOrNoneAndFailsClosed(t *testing.T) {
	s := fixtureStore(t)
	installFeed(t, s)
	feed := NewPluginFeed(s, "fake-notifier")
	for _, row := range []struct {
		value string
		ok    bool
	}{{"all", true}, {"none", true}, {"", false}, {"ALL", false}, {"all ", false}, {"*", false}, {"any", false}} {
		if err := feed.ConfigurePluginScope(row.value); (err == nil) != row.ok {
			t.Errorf("ConfigurePluginScope(%q) = %v", row.value, err)
		}
	}
	session, _ := register(t, s, "codex")
	for _, scope := range []string{"all", "none", "", "ALL", "all ", "*"} {
		inView(t, s, func(view bus.JournalView) {
			if got := standardMember(view, pluginRecord{Name: "fake-notifier", Scope: scope}, session.ID); got != (scope == "all") {
				t.Errorf("scope %q: member = %v", scope, got)
			}
		})
	}
}

// A member is a registered session that is not retired. A retired session,
// a session that was never registered and a plugin address are not.
func TestPluginMemberIsALiveSession(t *testing.T) {
	s := fixtureStore(t)
	live, _ := register(t, s, "codex")
	retired, _ := register(t, s, "codex")
	if err := s.Update(retired.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{
		live.ID:                     true,
		retired.ID:                  false,
		"codex:" + model.NewUUID():  false,
		"plugin:" + model.NewUUID(): false,
	} {
		inView(t, s, func(view bus.JournalView) {
			member, _ := view.PluginMember(id, "fake-notifier")
			if member != want {
				t.Errorf("%s: member = %v, want %v", id, member, want)
			}
		})
	}
}

// All five checks use the same membership. Each row is a session and a
// record; every check must allow the first row and deny every other.
func TestFiveChecksUseTheSameMembership(t *testing.T) {
	s := fixtureStore(t)
	live, _ := register(t, s, "codex")
	retired, _ := register(t, s, "codex")
	if err := s.Update(retired.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	all := func(a PluginAccess) pluginRecord {
		p := pluginRecord{ID: model.NewUUID(), Name: "fake-notifier", Launch: &PluginLaunch{PinSHA256: testSHA}, Enabled: true, Scope: scopeAll, Access: a}
		p.Revision = pluginRevision(p)
		return p
	}
	granted := func(id string) PluginAccess {
		return PluginAccess{Status: true, Receive: true, Send: []string{id}, Read: []string{id}}
	}
	none := all(granted(live.ID))
	none.Scope = "none"
	none.Revision = pluginRevision(none)
	unknown := "codex:" + model.NewUUID()
	rows := []struct {
		name    string
		session string
		p       pluginRecord
		allowed bool
	}{
		{"live member", live.ID, all(granted(live.ID)), true},
		{"retired", retired.ID, all(granted(retired.ID)), false},
		{"never registered", unknown, all(granted(unknown)), false},
		{"kind not granted", live.ID, all(PluginAccess{}), false},
	}
	for _, row := range rows {
		inView(t, s, func(view bus.JournalView) {
			for check, got := range fiveChecks(view, row.p, row.session) {
				if got != row.allowed {
					t.Errorf("%s: check %s = %v", row.name, check, got)
				}
			}
		})
	}
	// Scope none removes standard access only; named extended access stays.
	inView(t, s, func(view bus.JournalView) {
		got := fiveChecks(view, none, live.ID)
		for check, want := range map[string]bool{"A feed": false, "B admission": false, "B entry": false, "C reply": false, "C send": true, "D/E read": true} {
			if got[check] != want {
				t.Errorf("scope none: check %s = %v, want %v", check, got[check], want)
			}
		}
	})
}

// fiveChecks runs each use-time check for one session.
func fiveChecks(view bus.JournalView, p pluginRecord, session string) map[string]bool {
	status := PluginFeedStatus{ID: p.ID, Launch: p.Launch, Enabled: true, Revision: p.Revision, Epoch: p.Epoch}
	event := model.Event{Session: model.EventSession{ID: session, State: model.StateBusy, Previous: model.StateReady}}
	_, feed := feedView(feedMembers(view, p), p.Scope == scopeAll, event)
	return map[string]bool{
		"A feed":      p.Access.Status && feed,
		"B admission": pluginInboxAdmissionAllowed(view, p, session),
		"B entry":     inboxEntryState(view, p, pluginInboxEntry{Sender: session}) == "ok",
		"C reply":     mayAnswer(view, p, session),
		"C send":      extendedMember(view, p, p.Access.Send, session),
		"D/E read":    pluginReadAuthorization(view, p, &status, session) == "ok",
	}
}

// A session's own retirement or exit is offered once, judged by the
// membership before it; later events of that session are not.
func TestFeedOffersASessionsOwnEndOnce(t *testing.T) {
	gone := func(string) bool { return false }
	for name, test := range map[string]struct {
		e        model.EventSession
		scopeAll bool
		want     bool
	}{
		"retirement":               {model.EventSession{ID: "codex:x", Retired: true}, true, true},
		"owner exited":             {model.EventSession{ID: "codex:x", Cause: model.CauseOwnerExited}, true, true},
		"after retirement":         {model.EventSession{ID: "codex:x", PreviousRetired: true, Retired: true}, true, false},
		"exit after retirement":    {model.EventSession{ID: "codex:x", PreviousRetired: true, Retired: true, Cause: model.CauseOwnerExited}, true, false},
		"ordinary change":          {model.EventSession{ID: "codex:x", State: model.StateBusy}, true, false},
		"retirement in scope none": {model.EventSession{ID: "codex:x", Retired: true}, false, false},
	} {
		if _, got := feedView(gone, test.scopeAll, model.Event{Session: test.e}); got != test.want {
			t.Errorf("%s: offered = %v", name, got)
		}
	}
}

// The revision covers every owner decision and nothing about sessions.
func TestRevisionCoversOwnerChoicesOnly(t *testing.T) {
	base := pluginRecord{ID: model.NewUUID(), Launch: &PluginLaunch{PinSHA256: testSHA}, Scope: scopeAll, Access: PluginAccess{Status: true, Receive: true, Send: []string{"codex:a"}, Read: []string{"codex:b"}}}
	want := pluginRevision(base)
	if pluginRevision(base) != want || !validPluginSHA(want) {
		t.Fatal("revision is not a stable sha256")
	}
	for name, change := range map[string]func(*pluginRecord){
		"installation": func(p *pluginRecord) { p.ID = model.NewUUID() },
		"pin":          func(p *pluginRecord) { p.Launch = &PluginLaunch{PinSHA256: "1" + testSHA[1:]} },
		"scope":        func(p *pluginRecord) { p.Scope = "none" },
		"status":       func(p *pluginRecord) { p.Access.Status = false },
		"receive":      func(p *pluginRecord) { p.Access.Receive = false },
		"send":         func(p *pluginRecord) { p.Access.Send = nil },
		"read":         func(p *pluginRecord) { p.Access.Read = []string{"codex:c"} },
	} {
		p := base
		p.Access = base.Access.clone()
		change(&p)
		if pluginRevision(p) == want {
			t.Errorf("a %s change keeps the revision", name)
		}
	}
	for name, change := range map[string]func(*pluginRecord){
		"enabled":  func(p *pluginRecord) { p.Enabled = !p.Enabled },
		"epoch":    func(p *pluginRecord) { p.Epoch = model.NewUUID() },
		"position": func(p *pluginRecord) { p.Position.Sequence++ },
	} {
		p := base
		change(&p)
		if pluginRevision(p) != want {
			t.Errorf("a %s change moves the revision", name)
		}
	}
}

// A session start or retirement never changes the binding, so a running
// child is not stopped; an owner change does.
func TestMembershipChangesKeepTheBinding(t *testing.T) {
	s := fixtureStore(t)
	session, _ := register(t, s, "codex")
	enableFeed(t, s)
	feed := NewPluginFeed(s, "fake-notifier")
	launch, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	other, _ := register(t, s, "codex")
	if err := s.Update(session.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(other.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if code := feed.PluginBindingCode(&launch); code != "ok" {
		t.Fatalf("a membership change stopped the child: %s", code)
	}
	for _, step := range []func() error{
		func() error { return feed.EnablePluginFeed(false) },
		func() error { return feed.ConfigurePluginScope("none") },
		func() error { return feed.EnablePluginFeed(true) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if code := feed.PluginBindingCode(&launch); code != "stale_binding" {
		t.Fatalf("an owner scope change kept the child: %s", code)
	}
}

// A stored revision that does not match the record makes it invalid: a hand
// edit of the scope or access cannot keep the old binding.
func TestTamperedRevisionIsRefused(t *testing.T) {
	s := fixtureStore(t)
	register(t, s, "codex")
	enableFeed(t, s)
	feed := NewPluginFeed(s, "fake-notifier")
	raw, err := os.ReadFile(feed.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record["scope"] = "none"
	edited, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(feed.pluginPath(), edited, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := feed.PluginStatus(); err == nil || errors.Is(err, ErrOldPluginRecord) {
		t.Fatalf("tampered record loaded: %v", err)
	}
}

// Without status access there is no feed, even with scope all.
func TestNoFeedWithoutStatusAccess(t *testing.T) {
	s := fixtureStore(t)
	installFeed(t, s)
	feed := NewPluginFeed(s, "fake-notifier")
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(PluginAccess{Receive: true}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	register(t, s, "codex")
	if d := feedNext(t, s); d.ID != "" {
		t.Fatalf("a plugin without status access got a feed offer: %+v", d)
	}
}

// The membership probe runs under the journal lock, as zalki list does. It
// is syscalls only (no exec). Worst case for one locked step: 64 events of 64
// different sessions, each probed (a real process lookup of a recorded owner
// that has exited) and each denied, so one NextPluginFeed scans all 64 and
// offers nothing. That one locked step must stay far below the 250 ms poll.
func TestFeedScanOf64SessionsHoldsTheLockBriefly(t *testing.T) {
	s := fixtureStore(t)
	enableFeed(t, s)
	start := feedRecord(t, s).Position.Sequence
	for range 64 {
		session, _ := register(t, s, "codex")
		path, _ := sessionTestPath(s, session.ID)
		session.Owner = &model.ProcessRef{PID: 999999, Start: "gone"} // above any pid: the lookup says exited
		if err := writeJSON(path, session); err != nil {
			t.Fatal(err)
		}
	}
	feed := NewPluginFeed(s, "fake-notifier")
	launch, err := feed.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	d, err := feed.NextPluginFeed(&launch)
	took := time.Since(started)
	t.Logf("one locked feed scan of 64 denied sessions: %v", took)
	if err != nil || d.ID != "" {
		t.Fatalf("an exited session's event was offered: %+v %v", d, err)
	}
	if got := feedRecord(t, s).Position.Sequence; got != start+64 {
		t.Fatalf("the scan did not cover 64 events in one step: %d", got-start)
	}
	if took > 50*time.Millisecond*raceSlowdown {
		t.Fatalf("one locked scan took %v with 64 probed sessions", took)
	}
}
