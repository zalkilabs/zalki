package adapters_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func TestSessionPluginRealCursorRecoveryPolicies(t *testing.T) {
	for _, event := range []string{"postToolUse", "stop"} {
		for _, env := range []string{"unset", "off", "external", "invalid setting"} {
			t.Run(event+"/"+env, func(t *testing.T) { assertRealCursorPluginRecovery(t, event, env) })
		}
	}
}

func assertRealCursorPluginRecovery(t *testing.T, event, env string) {
	t.Helper()
	t.Setenv("ZALKI_PLUGINS", env)
	if env == "unset" {
		if err := os.Unsetenv("ZALKI_PLUGINS"); err != nil {
			t.Fatal(err)
		}
	}
	fixture := newResumedCursorFixture(t)
	installRecoveryStatusPlugin(t, fixture.service.Store)
	calls := 0
	fixture.service.PluginStartup = func(v bus.JournalView, root string, current model.Session, reserve bool, fits func(string) (bool, error)) (string, error) {
		calls++
		if reserve {
			t.Fatal("Z6_REAL_RECOVERY_SUMMARY_ONLY_PHASE")
		}
		return pluginfeed.SessionPluginStartup(v, root, current, reserve, fits)
	}
	t.Setenv("ZALKI_PLUGINS", "off")
	if out := cursorHook(t, fixture.service, fixture.actor.ID, "beforeSubmitPrompt", "", 0); strings.TrimSpace(out) != `{"continue":true}` || calls != 0 {
		t.Fatal("Z6_REAL_PROMPT_NO_SETUP_DISCLOSURE")
	}
	out := cursorHook(t, fixture.service, fixture.actor.ID, event, "completed", 0)
	assertRealCursorPluginSummary(t, out, env, calls)
	current := adapterTestMessage(t, fixture.service.Store, fixture.message.ID)
	if strings.Contains(out, fixture.message.Body) || current.Status != fixture.message.Status || current.AcknowledgedAt != "" {
		t.Fatal("Z6_REAL_RECOVERY_DID_NOT_DRAIN_PENDING")
	}
	ledgers, err := filepath.Glob(filepath.Join(fixture.service.Store.Dir, "plugins", "*.start-notice.json"))
	if err != nil || len(ledgers) != 0 {
		t.Fatal("Z6_REAL_RECOVERY_NO_NOTICE_SPEND", err)
	}
}

func assertRealCursorPluginSummary(t *testing.T, out, env string, calls int) {
	t.Helper()
	want := "Plugins active for this session: external."
	if env == "off" || env == "invalid setting" {
		want = "Plugins active for this session: none (ZALKI_PLUGINS=off)."
	}
	if calls != 1 || !strings.Contains(out, want) || len(out) > 24*1024 {
		t.Fatal("Z6_REAL_RECOVERY_TRUTHFUL_POLICY_SUMMARY", env)
	}
	if strings.Contains(out, "Invalid ZALKI_PLUGINS;") != (env == "invalid setting") {
		t.Fatal("Z6_REAL_RECOVERY_INVALID_ENV_DISCLOSED")
	}
}

func installRecoveryStatusPlugin(t *testing.T, store bus.Store) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "external")
	if err := feed.AddExternalPlugin(executable, nil); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(pluginfeed.PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
}
