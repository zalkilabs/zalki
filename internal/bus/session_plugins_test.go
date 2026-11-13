package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func pluginMemberForTest(t *testing.T, s Store, id, name string) bool {
	t.Helper()
	var result bool
	if err := WithJournalRead(s, func(v JournalView) error { var err error; result, err = v.PluginMember(id, name); return err }); err != nil {
		t.Fatal(err)
	}
	return result
}

func pluginPolicyForTest(t *testing.T, s Store, id string) (model.Session, sessionPluginPolicy) {
	t.Helper()
	v, err := s.session(id)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := decodeSessionPluginPolicy(v)
	if !ok {
		t.Fatal("Z6_VALID_PERSISTED_POLICY")
	}
	return v, p
}

func TestSessionPluginEnvironmentPredicate(t *testing.T) {
	tests := []struct {
		value             string
		arbiter, notifier bool
	}{
		{"", true, true}, {"off", false, false}, {"arbiter", true, false}, {"notifier,arbiter,arbiter", true, true},
		{"notifier", false, true}, {" ", false, false}, {"OFF", false, false}, {"arbiter,", false, false}, {"arbiter, notifier", false, false},
		{"*", false, false}, {"a/b", false, false}, {"off,arbiter", true, false}, {"off,notifier", false, true}, {"arbiter\n", false, false}, {"\xff", false, false}, {strings.Repeat("a", 4097), false, false}, {strings.Repeat("a,", 64) + "a", false, false},
	}
	for i, row := range tests {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			t.Setenv("ZALKI_PLUGINS", row.value)
			s := fixtureStore(t)
			v, _ := register(t, s, "codex")
			if pluginMemberForTest(t, s, v.ID, "arbiter") != row.arbiter || pluginMemberForTest(t, s, v.ID, "notifier") != row.notifier {
				t.Fatal("Z6_ENV_CURRENT_PREDICATE")
			}
			_, p := pluginPolicyForTest(t, s, v.ID)
			if p.LastChange.Actor != v.ID || p.LastChange.ID == "" {
				t.Fatal("Z6_INITIAL_AUDIT_BEFORE_PUBLICATION")
			}
		})
	}
}

func TestSessionPluginSelfOnlyAndCeiling(t *testing.T) {
	t.Setenv("ZALKI_PLUGINS", "arbiter")
	s := fixtureStore(t)
	a, key := register(t, s, "codex")
	b, _ := register(t, s, "cursor")
	if SetSessionPlugin(s, b.ID, key, "arbiter", true) == nil {
		t.Fatal("Z6_CANNOT_MUTE_OTHER_ACTOR")
	}
	if err := SetSessionPlugin(s, a.ID, key, "arbiter", true); err != nil {
		t.Fatal(err)
	}
	_, first := pluginPolicyForTest(t, s, a.ID)
	if pluginMemberForTest(t, s, a.ID, "arbiter") {
		t.Fatal("Z6_MUTE_TAKES_EFFECT")
	}
	if err := SetSessionPlugin(s, a.ID, key, "arbiter", true); err != nil {
		t.Fatal(err)
	}
	_, same := pluginPolicyForTest(t, s, a.ID)
	if same.LastChange.ID != first.LastChange.ID {
		t.Fatal("Z6_NOOP_KEEPS_AUDIT")
	}
	if err := SetSessionPlugin(s, a.ID, key, "arbiter", false); err != nil {
		t.Fatal(err)
	}
	_, last := pluginPolicyForTest(t, s, a.ID)
	if last.LastChange.ID == first.LastChange.ID || !pluginMemberForTest(t, s, a.ID, "arbiter") || pluginMemberForTest(t, s, a.ID, "notifier") {
		t.Fatal("Z6_UNMUTE_ENV_CEILING_AND_FRESH_AUDIT")
	}
}

func TestSessionPluginAuthenticationHasNoOracle(t *testing.T) {
	s := fixtureStore(t)
	a, key := register(t, s, "codex")
	b, _ := register(t, s, "cursor")
	for _, id := range []string{b.ID, "cursor:" + newID()} {
		err := SetSessionPlugin(s, id, key, "arbiter", true)
		if err == nil || !strings.HasSuffix(err.Error(), pluginAuthenticationFailed) {
			t.Fatal("Z6_UNPROVEN_AUTH_IS_UNIFORM", err)
		}
	}
	if err := s.Update(b.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if err := SetSessionPlugin(s, b.ID, key, "arbiter", true); err == nil || !strings.HasSuffix(err.Error(), pluginAuthenticationFailed) {
		t.Fatal("Z6_UNPROVEN_AUTH_IS_UNIFORM", err)
	}
	if err := SetSessionPlugin(s, a.ID, key, "bad name", true); err == nil {
		t.Fatal("Z6_PLUGIN_NAME_VALIDATED")
	}
}

func TestSessionPluginRetentionAndCorruptRepair(t *testing.T) {
	s := fixtureStore(t)
	v, key := register(t, s, "cursor")
	if err := SetSessionPlugin(s, v.ID, key, "arbiter", true); err != nil {
		t.Fatal(err)
	}
	_, old := pluginPolicyForTest(t, s, v.ID)
	t.Setenv("ZALKI_PLUGINS", "off")
	compact, _, err := s.register("cursor", v.NativeID, "/workspace", nil, true)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := decodeSessionPluginPolicy(compact)
	if !ok || p.Mode != "owner" || p.LastChange.ID != old.LastChange.ID {
		t.Fatal("Z6_COMPACTION_RETAINS_WHOLE_POLICY")
	}
	replaced, _, err := s.Register("cursor", v.NativeID, "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok = decodeSessionPluginPolicy(replaced)
	if !ok || p.Mode != "off" || p.LastChange.ID != old.LastChange.ID || p.Muted["arbiter"].Actor != v.ID {
		t.Fatal("Z6_REPLACEMENT_RETAINS_LOCKED_OVERLAY")
	}
}

func TestSessionPluginCorruptRepair(t *testing.T) {
	s := fixtureStore(t)
	replaced, _ := register(t, s, "cursor")
	v := replaced
	replaced.PluginPolicy = json.RawMessage(`{"mode":"owner"}`)
	path, _ := s.sessionPath(v.ID)
	if err := writeJSON(path, replaced); err != nil {
		t.Fatal(err)
	}
	if pluginMemberForTest(t, s, v.ID, "arbiter") {
		t.Fatal("Z6_CORRUPT_POLICY_IS_CLOSED")
	}
	t.Setenv("ZALKI_PLUGINS", "")
	if _, _, err := s.Register("cursor", v.NativeID, "/workspace", nil); err == nil {
		t.Fatal("Z6_CORRUPT_REPAIR_REQUIRES_OFF")
	}
	var log bytes.Buffer
	s.Logger = slog.New(slog.NewJSONHandler(&log, nil))
	t.Setenv("ZALKI_PLUGINS", "off")
	repaired, _, err := s.Register("cursor", v.NativeID, "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := decodeSessionPluginPolicy(repaired); !ok {
		t.Fatal("Z6_OFF_REPAIR_INITIALIZES_AUDIT")
	}
	if !strings.Contains(log.String(), "session.plugin_policy_repair_requested") {
		t.Fatal("Z6_OFF_REPAIR_BOUNDED_DIAGNOSTIC")
	}
}

func TestSessionPluginEligibilityAudit(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "cursor")
	_, p := pluginPolicyForTest(t, s, v.ID)
	original := p.LastChange.ID
	for _, state := range []model.SessionState{model.StateBusy, model.StateReady, model.StateApproval} {
		if err := s.Update(v.ID, state, false); err != nil {
			t.Fatal(err)
		}
		_, p = pluginPolicyForTest(t, s, v.ID)
		if p.LastChange.ID != original {
			t.Fatal("Z6_NO_AUDIT_ON_ORDINARY_STATE")
		}
	}
	for _, owner := range []*model.ProcessRef{{PID: 800001, Start: "A", Role: "owner"}, {PID: 800002, Start: "B", Role: "owner"}, {PID: 800001, Start: "A", Role: "owner"}} {
		current, _ := s.session(v.ID)
		current.Owner = owner
		if err := s.locked(func() error { return saveSessionFor(s, current, "") }); err != nil {
			t.Fatal(err)
		}
		_, p = pluginPolicyForTest(t, s, v.ID)
		if p.LastChange.ID == original {
			t.Fatal("Z6_OWNER_CHANGE_ROTATES_AUDIT")
		}
		original = p.LastChange.ID
		current, _ = s.session(v.ID)
		current.Owner = &model.ProcessRef{PID: owner.PID, Start: owner.Start, Role: owner.Role}
		current.Name = "same owner"
		current.Task = "task"
		current.KeyHash = "changed-key"
		if err := s.locked(func() error { return saveSessionFor(s, current, "") }); err != nil {
			t.Fatal(err)
		}
		_, p = pluginPolicyForTest(t, s, v.ID)
		if p.LastChange.ID != original {
			t.Fatal("Z6_EQUAL_OWNER_AND_TEXT_KEEP_AUDIT")
		}
	}
	if err := s.Update(v.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	_, p = pluginPolicyForTest(t, s, v.ID)
	if p.LastChange.ID == original {
		t.Fatal("Z6_RETIREMENT_ROTATES_AUDIT")
	}
	original = p.LastChange.ID
	h := hookRuntime{store: s, actor: v.ID, kind: "cursor", event: "UserPromptSubmit"}
	if err := h.ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate}, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, p = pluginPolicyForTest(t, s, v.ID)
	if p.LastChange.ID == original {
		t.Fatal("Z6_REACTIVATION_ROTATES_AUDIT")
	}
}

func TestSessionPluginPublishedFailureAndRetry(t *testing.T) {
	s := fixtureStore(t)
	v, key := register(t, s, "codex")
	writer := sessionPluginWriter{save: func(store Store, current model.Session) error {
		if err := saveSessionFor(store, current, ""); err != nil {
			return err
		}
		return errors.New("post-publication sync failure")
	}, sync: journal.SyncDir}
	if err := setSessionPlugin(s, v.ID, key, "arbiter", true, writer); err == nil || !strings.HasSuffix(err.Error(), pluginChangeUncertain) {
		t.Fatal("Z6_PUBLISHED_FAILURE_IS_UNCERTAIN", err)
	}
	_, p := pluginPolicyForTest(t, s, v.ID)
	id := p.LastChange.ID
	if pluginMemberForTest(t, s, v.ID, "arbiter") {
		t.Fatal("Z6_PUBLISHED_POLICY_IS_CURRENT_TRUTH")
	}
	calls := 0
	writer.sync = func(string) error { calls++; return errors.New("sync failed") }
	if err := setSessionPlugin(s, v.ID, key, "arbiter", true, writer); err == nil || calls != 1 {
		t.Fatal("Z6_NOOP_RETRY_MUST_SYNC", err, calls)
	}
	if err := SetSessionPlugin(s, v.ID, key, "arbiter", true); err != nil {
		t.Fatal(err)
	}
	_, p = pluginPolicyForTest(t, s, v.ID)
	if p.LastChange.ID != id {
		t.Fatal("Z6_RETRY_DOES_NOT_REWRITE_AUDIT")
	}
}
