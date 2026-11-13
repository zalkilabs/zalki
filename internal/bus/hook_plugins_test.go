package bus

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionPluginStartupPortScopeAndGeneration(t *testing.T) {
	s := fixtureStore(t)
	current, key := register(t, s, "cursor")
	var captured JournalView
	calls := 0
	service := Service{Store: s, PluginStartup: func(v JournalView, root string, session model.Session, reserve bool, fits func(string) (bool, error)) (string, error) {
		captured = v
		calls++
		if root != s.Dir || session.Generation != current.Generation || !reserve {
			t.Fatal("Z6_STARTUP_PORT_PHASE_GENERATION")
		}
		if err := v.WithWriteScope(root, func() error { return nil }); err != nil {
			t.Fatal("Z6_STARTUP_SAME_WRITE_SCOPE", err)
		}
		return "Plugins active for this session: none.", nil
	}}
	var out bytes.Buffer
	if err := service.sessionStartupOutput(stubCursorAdapter{}, current, key, true, &out); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !strings.Contains(out.String(), "Plugins active for this session: none.") {
		t.Fatal("Z6_STARTUP_MANDATORY_DISCLOSURE")
	}
	if err := captured.WithWriteScope(s.Dir, func() error { return nil }); err == nil {
		t.Fatal("Z6_STARTUP_ESCAPED_VIEW_EXPIRES")
	}
	if _, _, err := s.Register("cursor", current.NativeID, current.CWD, nil); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := service.sessionStartupOutput(stubCursorAdapter{}, current, key, true, &out); err == nil || calls != 1 || out.Len() != 0 {
		t.Fatal("Z6_STALE_STARTUP_NO_OUTPUT_OR_DEBIT")
	}
}

type expandedStartupHook struct {
	agent.StandardHooks
	padding string
}

func (h expandedStartupHook) StartupOutput(v agent.StartupContext) (any, error) {
	return map[string]string{"padding": h.padding, "context": v.Instructions}, nil
}

func TestSessionPluginFullJSONBudgetBeforeNotice(t *testing.T) {
	s := fixtureStore(t)
	current, key := register(t, s, "cursor")
	selected := 0
	service := Service{Store: s, PluginStartup: func(_ JournalView, _ string, _ model.Session, _ bool, fits func(string) (bool, error)) (string, error) {
		summary := "Plugins active for this session: none."
		if ok, err := fits(summary); err != nil || !ok {
			return summary, errors.New("mandatory too large")
		}
		candidate := summary + "\nNew plugin: " + strings.Repeat("<", 350)
		if ok, err := fits(candidate); err != nil || ok {
			t.Fatal("Z6_FULL_JSON_ESCAPE_FIT_BEFORE_DEBIT", err)
		}
		selected++
		return summary, nil
	}}
	var out bytes.Buffer
	if err := service.sessionStartupOutput(expandedStartupHook{}, current, key, true, &out); err != nil || selected != 1 || strings.Contains(out.String(), "New plugin:") {
		t.Fatal("Z6_OMITTED_WHOLE_NOTICE", err)
	}
	out.Reset()
	if err := service.sessionStartupOutput(expandedStartupHook{padding: strings.Repeat("x", hookContextBytes)}, current, key, true, &out); err != nil || out.Len() != 0 {
		t.Fatal("Z6_OVERFULL_MANDATORY_SAFE_NO_OUTPUT", err)
	}
}

func TestSessionPluginRecoveryMandatoryDisclosureAndNoNotice(t *testing.T) {
	s := fixtureStore(t)
	current, _ := register(t, s, "cursor")
	var view JournalView
	h := hookRuntime{store: s, actor: current.ID, kind: "cursor", event: "PostToolUse", pluginStartup: func(v JournalView, _ string, session model.Session, reserve bool, fits func(string) (bool, error)) (string, error) {
		view = v
		if reserve || fits != nil || session.Generation != current.Generation {
			t.Fatal("Z6_RECOVERY_SUMMARY_ONLY_PHASE")
		}
		if err := v.WithWriteScope(s.Dir, func() error { return nil }); err != nil {
			t.Fatal("Z6_RECOVERY_SAME_WRITE_SCOPE", err)
		}
		return "Plugins active for this session: none (ZALKI_PLUGINS=off).", nil
	}}
	grant, err := h.ClaimRecovery(stubClaimRecovery)
	if err != nil || !strings.Contains(grant.Instructions, "Plugins active for this session: none (ZALKI_PLUGINS=off).") {
		t.Fatal("Z6_RECOVERY_MANDATORY_DISCLOSURE", err)
	}
	if err := view.WithWriteScope(s.Dir, func() error { return nil }); err == nil {
		t.Fatal("Z6_RECOVERY_VIEW_EXPIRES")
	}
	raw := sessionBytes(t, s, current.ID)
	if bytes.Contains(raw, []byte(grant.Capability)) || bytes.Contains(raw, []byte("Plugins active")) {
		t.Fatal("Z6_RECOVERY_SUMMARY_NOT_PERSISTED")
	}
}

type compactPluginCursor struct{ stubCursorAdapter }

func (compactPluginCursor) RegisterHook(v agent.HookInput) (agent.HookRegistration, error) {
	return agent.HookRegistration{PreserveGeneration: v.Source == "compact"}, nil
}

func TestSessionPluginCompactionDoesNotReserveNotice(t *testing.T) {
	s := fixtureStore(t)
	var err error
	s.Registry, err = agent.NewRegistry(compactPluginCursor{})
	if err != nil {
		t.Fatal(err)
	}
	current, _ := register(t, s, "cursor")
	calls := 0
	service := Service{Store: s, PluginStartup: func(_ JournalView, _ string, _ model.Session, reserve bool, _ func(string) (bool, error)) (string, error) {
		calls++
		if reserve {
			t.Fatal("Z6_COMPACTION_SUMMARY_ONLY_PHASE")
		}
		return "Plugins active for this session: none.", nil
	}}
	input := `{"session_id":"` + current.NativeID + `","cwd":"/workspace","hook_event_name":"sessionStart","source":"compact"}`
	var out bytes.Buffer
	if err := service.Hook(t.Context(), "cursor", strings.NewReader(input), &out); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestSessionPluginMissingStartupPortDisclosesUnavailable(t *testing.T) {
	s := fixtureStore(t)
	current, key := register(t, s, "cursor")
	var out bytes.Buffer
	if err := (&Service{Store: s}).sessionStartupOutput(stubCursorAdapter{}, current, key, true, &out); err != nil || !strings.Contains(out.String(), pluginStartupUnavailable) {
		t.Fatal("Z6_NIL_STARTUP_PORT_UNAVAILABLE_DISCLOSURE", err)
	}
}
