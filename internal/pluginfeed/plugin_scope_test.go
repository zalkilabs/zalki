package pluginfeed

import (
	"encoding/json"
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
	feed := streamSessionEventEligible(event.Session, streamGuard{}, standardMember(view, p, session))
	return map[string]bool{
		"A feed":      p.Access.Status && feed,
		"B admission": pluginInboxAdmissionAllowed(view, p, session),
		"B entry":     inboxEntryState(view, p, pluginInboxEntry{Sender: session}) == "ok",
		"C reply":     mayAnswer(view, p, session),
		"C send":      extendedMember(view, p, p.Access.Send, session),
		"D/E read":    pluginReadAuthorization(view, p, &status, session) == "ok",
	}
}

// No rich final event is offered without the later same-write witness, even
// when a membership callback still says true after an eligibility change.
func TestFeedSuppressesUnprovedRichFinalEvents(t *testing.T) {
	cases := []model.EventSession{
		{ID: "codex:x", Retired: true},
		{ID: "codex:x", Cause: model.CauseOwnerExited},
		{ID: "codex:x", Retired: true, Cause: model.CauseOwnerExited},
		{ID: "codex:x", PreviousRetired: true, Retired: true},
	}
	for _, event := range cases {
		for _, allowed := range []bool{true, false} {
			if streamSessionEventEligible(event, streamGuard{}, allowed) {
				t.Fatal("Z6_FINAL_EVENT_WITHOUT_WITNESS_HIDDEN")
			}
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
		"enabled": func(p *pluginRecord) { p.Enabled = !p.Enabled },
		"epoch":   func(p *pluginRecord) { p.Epoch = model.NewUUID() },
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
	if _, err := feed.PluginStatus(); err == nil {
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

// One locked step may inspect64 distinct denied records. Elapsed time is
// diagnostic only: native syscalls and host scheduling have no universal SLA.
func TestFeedScanOf64SessionsHoldsTheLockBriefly(t *testing.T) {
	store := fixtureStore(t)
	enableFeed(t, store)
	feed := fixtureFeedRun(NewPluginFeed(store, "fake-notifier"))
	launch := streamTestLaunch(t, feed)
	if d := streamInspect(t, feed, launch); d.ID != "" {
		t.Fatal("private cut emitted unexpected frame")
	}
	start := readStreamCheckpoint(t, feed).Cursor
	addDeniedStreamScanFacts(t, store, 64)
	measured := recordStreamScanWork(t, feed)
	began := time.Now()
	delivery, err := feed.NextPluginFeed(t.Context(), launch, "", 0)
	t.Logf("one locked feed step of64 denied sessions: %v (diagnostic only)", time.Since(began))
	if err != nil || delivery.ID != "" {
		t.Fatal("exited session fact was offered", delivery, err)
	}
	current := readStreamCheckpoint(t, feed).Cursor
	if current.Sequence != start.Sequence+64 || current.Epoch != eventState(t, store).Epoch {
		t.Fatalf("scan did not cover exactly64 events: %+v -> %+v", start, current)
	}
	assertStreamScanWork(t, measured, 0, 64)
}
