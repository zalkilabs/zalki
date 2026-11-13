package pluginfeed

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func startupText(t *testing.T, f PluginFeed, s model.Session, reserve bool, fits func(string) (bool, error)) string {
	t.Helper()
	var text string
	err := bus.WithJournalWrite(f.store, func(v bus.JournalView) error {
		var err error
		text, err = SessionPluginStartup(v, f.store.Dir, s, reserve, fits)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestSessionStartupTruthAndScopeCeiling(t *testing.T) {
	f, _, _ := externalPinFixture(t)
	if err := f.GrantPluginAccess(PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	s, key := register(t, f.store, "cursor")
	text := startupText(t, f, s, false, nil)
	if text != "Plugins active for this session: external." {
		t.Fatal("Z6_STARTUP_TRUTHFUL_ACTIVE", text)
	}
	if err := bus.SetSessionPlugin(f.store, s.ID, key, "external", true); err != nil {
		t.Fatal(err)
	}
	if text := startupText(t, f, s, false, nil); text != "Plugins active for this session: none." {
		t.Fatal("Z6_STARTUP_CURRENT_MUTE", text)
	}
	for _, env := range []string{"off", "external", "other", " bad "} {
		t.Run(env, func(t *testing.T) {
			t.Setenv("ZALKI_PLUGINS", env)
			s, _ := register(t, f.store, "cursor")
			text := startupText(t, f, s, false, nil)
			active := env == "external"
			if strings.Contains(text, "external.") != active {
				t.Fatal("Z6_STARTUP_ENV_SUMMARY", text)
			}
			if strings.Contains(text, "Invalid ZALKI_PLUGINS") != (env == " bad ") {
				t.Fatal("Z6_STARTUP_INVALID_DIAGNOSTIC", text)
			}
		})
	}
	if err := bus.SetSessionPlugin(f.store, s.ID, key, "external", false); err != nil {
		t.Fatal(err)
	}
	if err := f.ConfigurePluginScope("none"); err != nil {
		t.Fatal(err)
	}
	if text := startupText(t, f, s, false, nil); strings.Contains(text, "external.") {
		t.Fatal("Z6_STARTUP_OWNER_CEILING", text)
	}
}

func TestSessionStartupNoticeFitBeforeDebitAndUnproven(t *testing.T) {
	f, p := startNoticeFixture(t)
	p.Install.InstalledBy = "unproven"
	p.Install.InstalledByLabel = ""
	if _, err := f.savePlugin(p); err != nil {
		t.Fatal(err)
	}
	s, _ := register(t, f.store, "cursor")
	text := startupText(t, f, s, true, func(text string) (bool, error) { return !strings.Contains(text, "New plugin:"), nil })
	if strings.Contains(text, "New plugin:") {
		t.Fatal("Z6_WHOLE_OMITTED_NOTICE_NO_OUTPUT")
	}
	if _, err := os.Stat(f.startNoticePath()); !os.IsNotExist(err) {
		t.Fatal("Z6_OMITTED_NOTICE_NO_DEBIT", err)
	}
	text = startupText(t, f, s, true, func(string) (bool, error) { return true, nil })
	if !strings.Contains(text, "by an unverified installer. See: zalki plugins list") {
		t.Fatal("Z6_UNPROVEN_NOTICE_ELIGIBLE", text)
	}
	ledger, err := f.loadStartNotice(p)
	if err != nil || len(ledger.Starts) != 1 {
		t.Fatal("Z6_SELECTED_NOTICE_DURABLE_DEBIT", err)
	}
	text = startupText(t, f, s, true, func(string) (bool, error) { return true, nil })
	if strings.Contains(text, "New plugin:") {
		t.Fatal("Z6_DUPLICATE_START_NO_NOTICE")
	}
}

func TestSessionStartupThreeStartsAndRecoveryNoSpend(t *testing.T) {
	f, p := startNoticeFixture(t)
	first, _ := register(t, f.store, "cursor")
	if text := startupText(t, f, first, false, nil); strings.Contains(text, "New plugin:") {
		t.Fatal("Z6_RECOVERY_SUMMARY_ONLY")
	}
	if _, err := os.Stat(f.startNoticePath()); !os.IsNotExist(err) {
		t.Fatal("Z6_RECOVERY_NOTICE_NO_SPEND", err)
	}
	for i := range 4 {
		s, _ := register(t, f.store, "cursor")
		text := startupText(t, f, s, true, func(string) (bool, error) { return true, nil })
		if strings.Contains(text, "New plugin:") != (i < 3) {
			t.Fatal("Z6_THREE_REAL_STARTS_LIMIT", i, text)
		}
	}
	ledger, err := f.loadStartNotice(p)
	if err != nil || len(ledger.Starts) != 3 {
		t.Fatal("Z6_GLOBAL_NOTICE_LIMIT", err)
	}
}

func TestSessionStartupSortedBoundedSummary(t *testing.T) {
	f := NewPluginFeed(fixtureStore(t), "external")
	session, _ := register(t, f.store, "cursor")
	records := make([]pluginRecord, 0, 18)
	for i := 17; i >= 0; i-- {
		records = append(records, pluginRecord{Name: fmt.Sprintf("p%02d", i), Enabled: true, Scope: scopeAll, Access: PluginAccess{Status: true}})
	}
	var text string
	if err := bus.WithJournalRead(f.store, func(v bus.JournalView) error { text = sessionStartupSummary(v, session, records); return nil }); err != nil {
		t.Fatal(err)
	}
	want := "Plugins active for this session: p00, p01, p02, p03, p04, p05, p06, p07, p08, p09, p10, p11, p12, p13, p14, p15, +2 more. See: zalki plugins list"
	if text != want {
		t.Fatal("Z6_SORTED_SIXTEEN_WITH_OVERFLOW", text)
	}
}

func TestSessionStartupPublishedDebitIsNeverRefunded(t *testing.T) {
	f, p := startNoticeFixture(t)
	session, _ := register(t, f.store, "cursor")
	err := bus.WithJournalWrite(f.store, func(bus.JournalView) error {
		text, err := appendSessionStartNoticesWith([]pluginRecord{p}, "mandatory summary", func(string) (bool, error) { return true, nil }, func(p pluginRecord, now time.Time) (bool, error) {
			return f.reserveStartNoticeWith(p, session.ID, session.Generation, now, func(path string, raw []byte) error {
				if err := journal.WriteAtomic(path, raw); err != nil {
					return err
				}
				return errors.New("published debit, output not emitted")
			})
		})
		if err == nil || strings.Contains(text, "New plugin:") {
			t.Fatal("Z6_UNCERTAIN_NOTICE_NO_SUCCESS_LINE")
		}
		ledger, err := f.loadStartNotice(p)
		if err != nil || len(ledger.Starts) != 1 {
			t.Fatal("Z6_PUBLISHED_NOTICE_DEBIT_NEVER_REFUNDED", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		s, _ := register(t, f.store, "cursor")
		text := startupText(t, f, s, true, func(string) (bool, error) { return true, nil })
		if strings.Contains(text, "New plugin:") != (i < 2) {
			t.Fatal("Z6_OUTPUT_LOSS_STILL_LIMITS_THREE_NOTICES", i)
		}
	}
}
