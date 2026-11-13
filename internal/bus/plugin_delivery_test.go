package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func pendingPluginFixture(t *testing.T) (Store, model.Session, string, model.Message) {
	t.Helper()
	s := fixtureStore(t)
	to, key := register(t, s, "cursor")
	m := model.Message{Schema: recordSchema, ID: newID(), From: "plugin:" + newID(), FromName: "external", FromLabel: model.PluginLabel, To: to.ID, Body: "pending private plugin body", TargetGeneration: to.Generation, CreatedAt: model.Timestamp(), Status: model.StatusPendingActivity, PluginBinding: strings.Repeat("b", 64)}
	if err := s.locked(func() error { return s.saveMessage(m) }); err != nil {
		t.Fatal(err)
	}
	return s, to, key, m
}

func denyPluginDelivery(view JournalView, root string, _ model.Message) error {
	return view.WithWriteScope(root, func() error { return errors.New("current delivery denied") })
}

func TestSessionPluginFinalCoreHandoffs(t *testing.T) {
	s, to, _, m := pendingPluginFixture(t)
	s.PluginDelivery = denyPluginDelivery
	service := Service{Store: s}
	if _, _, err := service.revalidateTarget(m.ID, to); err == nil {
		t.Fatal("Z6_FINAL_TRANSPORT_CHECK")
	}
	err := s.locked(func() error {
		pending, err := s.pendingForHook(to.ID, to)
		if err != nil {
			return err
		}
		if len(pending) != 0 {
			t.Fatal("Z6_FINAL_PENDING_SELECTION_CHECK")
		}
		var out strings.Builder
		if err := s.emitPendingContext([]model.Message{m}, &out); err != nil {
			return err
		}
		if out.Len() != 0 {
			t.Fatal("Z6_FINAL_CONTEXT_EMISSION_CHECK")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.message(m.ID)
	if err != nil || stored.Status != model.StatusPendingActivity {
		t.Fatal("Z6_REFUSED_HANDOFF_STAYS_PENDING", err)
	}
}

func TestSessionPluginFinalListenerHandoff(t *testing.T) {
	s, to, key, m := pendingPluginFixture(t)
	lease := armedLease(t, s, to.ID, key)
	defer lease.Close(nil)
	lease.store.PluginDelivery = denyPluginDelivery
	if _, err := lease.ValidateDelivery(m.ID); err == nil {
		t.Fatal("Z6_FINAL_LISTENER_CHECK")
	}
	lease.store.PluginDelivery = func(v JournalView, root string, _ model.Message) error {
		return v.WithWriteScope(root, func() error { return nil })
	}
	if _, err := lease.ValidateDelivery(m.ID); err != nil {
		t.Fatal("listener positive", err)
	}
}

func TestSessionPluginFinalNativeQueries(t *testing.T) {
	s, to, key, m := pendingPluginFixture(t)
	s.PluginDelivery = denyPluginDelivery
	for _, query := range []MessageQuery{{Actor: to.ID, Capability: key}, {Actor: to.ID, Capability: key, History: true, IncludeBody: true}} {
		page, err := s.ReadMessages(query)
		if err != nil || len(page.Messages) != 0 {
			t.Fatal("Z6_FINAL_NATIVE_LIST_CHECK", err)
		}
		query.MessageID = m.ID
		if page, err := s.ReadMessages(query); err == nil || len(page.Messages) != 0 {
			t.Fatal("Z6_FINAL_NATIVE_EXACT_CHECK")
		}
	}
}

func TestSessionPluginBindingPersistenceAndRedaction(t *testing.T) {
	s, _, _, m := pendingPluginFixture(t)
	stored, err := s.message(m.ID)
	if err != nil || stored.PluginBinding != m.PluginBinding {
		t.Fatal("Z6_BINDING_SURVIVES_DISK", err)
	}
	raw, err := encodeStateJSON(m)
	if err != nil || !bytes.Contains(raw, []byte(m.PluginBinding)) {
		t.Fatal("Z6_DISK_HAS_ORIGINAL_BINDING", err)
	}
	projections := map[string]any{"message-json": m, "mcp-cli-list-get-api": MessageBatch{Messages: []model.Message{m}}, "audit": messageEvent(m)}
	event, err := messageStatusEvent(nil, m)
	if err != nil {
		t.Fatal(err)
	}
	projections["plugin-feed-event"] = event
	for name, value := range projections {
		raw, err := json.Marshal(value)
		if err != nil || bytes.Contains(raw, []byte("plugin_binding")) || bytes.Contains(raw, []byte(m.PluginBinding)) {
			t.Fatal("Z6_BINDING_ALL_JSON_PROJECTIONS_REDACTED", name, err)
		}
	}
	if strings.Contains(model.Envelope(m), m.PluginBinding) {
		t.Fatal("Z6_BINDING_NATIVE_CONTEXT_REDACTED")
	}
	rows, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(rows)
	if bytes.Contains(raw, []byte("plugin_policy")) {
		t.Fatal("Z6_POLICY_DISCOVERY_REDACTED")
	}
}

func TestSessionPluginMissingBindingCannotBecomeNative(t *testing.T) {
	s, _, _, m := pendingPluginFixture(t)
	m.PluginBinding = ""
	s.PluginDelivery = func(JournalView, string, model.Message) error { return nil }
	if checkPluginMessageDelivery(s, m) == nil {
		t.Fatal("Z6_MISSING_BINDING_IS_PLUGIN_ORIGIN")
	}
	m.From = "malformed"
	if checkPluginMessageDelivery(s, m) == nil {
		t.Fatal("Z6_MALFORMED_ORIGIN_CLOSED")
	}
	m.From = "cursor:" + newID()
	if checkPluginMessageDelivery(s, m) == nil {
		t.Fatal("Z6_PLUGIN_LABEL_CANNOT_NATIVE_BYPASS")
	}
	m.FromLabel = "Cursor"
	s.PluginDelivery = nil
	if err := checkPluginMessageDelivery(s, m); err != nil {
		t.Fatal("Z6_NATIVE_MESSAGE_NEEDS_NO_PLUGIN_CHECK", err)
	}
}

func TestSessionPluginNilFinalCheckerFailsClosed(t *testing.T) {
	s, _, _, m := pendingPluginFixture(t)
	if err := checkPluginMessageDelivery(s, m); err == nil {
		t.Fatal("Z6_NIL_FINAL_CHECKER_CLOSED")
	}
}

func TestSessionPluginRefusedAndAcknowledgedHistoryCannotBypass(t *testing.T) {
	for _, status := range []model.DeliveryStatus{model.StatusNotSent, model.StatusCommandFailed, model.StatusContextEmitted, model.StatusSubmitted} {
		s, to, key, m := pendingPluginFixture(t)
		s.PluginDelivery = denyPluginDelivery
		m.Status = status
		if status.CanAcknowledge() {
			m.AcknowledgedAt = model.Timestamp()
		}
		if err := s.locked(func() error { return s.saveMessage(m) }); err != nil {
			t.Fatal(err)
		}
		query := MessageQuery{Actor: to.ID, Capability: key, History: true, IncludeBody: true}
		page, err := s.ReadMessages(query)
		if err != nil || len(page.Messages) != 0 {
			t.Fatal("Z6_HISTORY_CANNOT_BYPASS_CURRENT_PLUGIN_POLICY", status, err)
		}
		query.MessageID = m.ID
		if _, err := s.ReadMessages(query); err == nil {
			t.Fatal("Z6_ACK_CANNOT_BYPASS_CURRENT_PLUGIN_POLICY", status)
		}
	}
}
