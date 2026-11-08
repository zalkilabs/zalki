package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// fourthAdapter is compiled outside package bus. It proves an external client
// can implement the exported adapter contract, including unexported optional
// interfaces, and be composed with the built-in registry.
type fourthAdapter struct{ agent.StandardHooks }

func (fourthAdapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{
		Kind: "fourth", Label: "Fourth", Hooks: agent.StandardEvents(true),
		ProcessNames: []string{"fourth"}, OwnerRole: "client", Executable: "fourth",
	}
}

// This separate client uses the same trusted lifecycle host without depending
// on Cursor's state format, credential operations or private journal helpers.
type fourthJournalAdapter struct{ fourthAdapter }

func (fourthJournalAdapter) ReceiveCapabilities(bus.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "fixture_hooks", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, LifecycleHooks: true, HookDelivery: true}
}

func (fourthJournalAdapter) Deliver(context.Context, bus.Message, bus.Session) bus.Receipt {
	return model.NewReceipt(model.StatusPendingActivity)
}

func (fourthJournalAdapter) HandleHook(host agent.HookHost, event agent.HookInput, id string, output io.Writer) (bool, error) {
	if event.Event != "PostToolUse" {
		return false, nil
	}
	if err := host.UpdateSession(id, model.StateBusy, false); err != nil {
		return true, err
	}
	text, err := host.EmitPending(id)
	if err != nil || text == "" {
		return true, err
	}
	return true, json.NewEncoder(output).Encode(map[string]string{"context": text})
}

func TestExternalAdapterUsesTrustedLifecycleHost(t *testing.T) {
	registry, err := agent.NewRegistry(fourthJournalAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	from, key, err := store.Register("fourth", "sender", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	to, toKey, err := store.Register("fourth", "receiver", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: "00000000-0000-4000-8000-000000000011", Body: "external lifecycle note"})
	if err != nil || msg.Status != model.StatusPendingActivity {
		t.Fatal("external client did not defer to its lifecycle hook", err)
	}
	input, err := json.Marshal(agent.HookInput{SessionID: "receiver", Event: "PostToolUse"})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := range 2 {
		var output bytes.Buffer
		if err := service.Hook(t.Context(), "fourth", bytes.NewReader(input), &output); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 && !strings.Contains(output.String(), msg.Body) {
			t.Fatal("external lifecycle host did not emit the pending envelope")
		}
		if attempt == 1 && output.Len() != 0 {
			t.Fatal("external hook emitted the same context twice")
		}
	}
	page, err := store.QueryMessages(bus.MessageQuery{Actor: to.ID, Capability: toKey, MessageID: msg.ID})
	if err != nil || len(page) != 1 || page[0].Status != model.StatusContextEmitted || page[0].AcknowledgedAt != "" {
		t.Fatal("lifecycle delivery forged acknowledgement or lost the receipt", err)
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ID == to.ID && session.State != model.StateBusy {
			t.Fatal("trusted lifecycle transition was lost")
		}
	}
}

func (fourthAdapter) ReceiveCapabilities(bus.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{
		Delivery: "external_log", Inbox: model.InboxNative, BusyDelivery: model.DeliveryNextTurn, LifecycleHooks: true,
	}
}

func (fourthAdapter) Deliver(context.Context, bus.Message, bus.Session) bus.Receipt {
	receipt := model.NewReceipt(model.StatusSubmitted)
	receipt.Output = "external-fourth"
	return receipt
}

func TestExternalAdapterComposesWithBuiltins(t *testing.T) {
	codex, err := adapters.Builtin().Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	claude, err := adapters.Builtin().Lookup("claude")
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := adapters.Builtin().Lookup("cursor")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewRegistry(codex, claude, cursor, fourthAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	if got := registry.Kinds(); len(got) != 4 || got[0] != "claude" || got[3] != "fourth" {
		t.Fatalf("kinds = %v", got)
	}
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	from, key, err := store.Register("codex", "native-sender", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	target, _, err := store.Register("fourth", "native-fourth", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	msg, err := service.Send(context.Background(), bus.SendRequest{
		Actor: from.ID, Capability: key, To: target.ID, MessageID: "00000000-0000-4000-8000-000000000010", Body: "hello from outside bus",
	})
	if err != nil || msg.Status != model.StatusSubmitted || msg.Receipt == nil || msg.Receipt.Output != "external-fourth" {
		t.Fatalf("external delivery was not accepted: %+v %v", msg, err)
	}
	files, err := registry.Configuration("/bin/zalki", store.Dir)
	if err != nil || len(files) == 0 {
		t.Fatal("built-in configuration disappeared beside the external adapter", err)
	}
}
