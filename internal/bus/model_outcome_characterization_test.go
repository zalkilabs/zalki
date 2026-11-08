package bus_test

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"

	agentapi "github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// Characterized, not endorsed: the completed message status wins the race,
// while its nested receipt still records the incoming transport outcome.
func TestModelLateWakeReceiptKeepsItsOwnStatus(t *testing.T) {
	store := newStore(t, nativeAdapter("alpha"), hookDeliveryAdapter("beta"))
	from, to := register(t, store, "alpha"), register(t, store, "beta")
	id := newUUID(t)
	if _, _, _, err := store.ReserveMessage(bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, MessageID: id, Body: "body"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(id, bus.Receipt{At: "pending", Status: model.StatusPendingActivity}); err != nil {
		t.Fatal(err)
	}
	if output, err := store.EmitPending(to.ID); err != nil || output == "" {
		t.Fatalf("hook did not emit pending context: %q, %v", output, err)
	}
	late := bus.Receipt{At: "late", Status: model.StatusWakeWritten, Error: "wake detail", Output: "native output"}
	msg, err := store.Finish(id, late)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Status != "hook_context_emitted" || msg.Receipt == nil {
		t.Fatalf("late wake replaced the message's emitted status: %+v", msg)
	}
	const wantReceipt = `{"at":"late","status":"wake_hint_written","code":"unavailable","error":"wake detail","output":"native output"}`
	requireModelJSON(t, msg.Receipt, wantReceipt)
	stored := readJSONFile(t, filepath.Join(store.Dir, "messages", id+".json"))
	if stored["status"] != "hook_context_emitted" {
		t.Fatalf("stored message status=%v", stored["status"])
	}
	// Generic map encoding sorts keys; pin the independently decoded record too.
	requireModelJSON(t, stored["receipt"], `{"at":"late","code":"unavailable","error":"wake detail","output":"native output","status":"wake_hint_written"}`)
}

// modelReleaseAdapter supplies one ordinary lifecycle event through the public
// SPI, so release is observable without calling private approval operations.
type modelReleaseAdapter struct {
	*fakeAdapter
	agentapi.StandardHooks
}

func (a modelReleaseAdapter) Descriptor() agentapi.AdapterDescriptor {
	d := a.fakeAdapter.Descriptor()
	d.Hooks = map[string]string{"stop": "Stop"}
	return d
}

func newModelHeldFixture(t *testing.T, hookDelivery bool) (bus.Service, *fakeAdapter, agent, bus.Message) {
	t.Helper()
	target := nativeAdapter("beta")
	if hookDelivery {
		target = hookDeliveryAdapter("beta")
	}
	target.caps.LifecycleHooks = true
	store := newStore(t, nativeAdapter("alpha"), modelReleaseAdapter{fakeAdapter: target})
	from, to := register(t, store, "alpha"), register(t, store, "beta")
	if err := store.Update(to.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	held, err := service.Send(t.Context(), bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "held body"})
	if err != nil {
		t.Fatal(err)
	}
	if !held.Held || held.Status != "not_sent_pending_approval" || held.Receipt == nil || held.Receipt.Error != "queued until the permission prompt is answered" {
		t.Fatalf("fixture was not held with its explanatory receipt: %+v", held)
	}
	return service, target, to, held
}

func releaseModelHeld(t *testing.T, service bus.Service, to agent) {
	t.Helper()
	input := fmt.Sprintf(`{"session_id":%q,"hook_event_name":"stop"}`, to.NativeID)
	if err := service.Hook(t.Context(), "beta", bytes.NewBufferString(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
}

func TestModelNativeReleaseClearsReceiptBeforeTransport(t *testing.T) {
	service, target, to, held := newModelHeldFixture(t, false)
	observedCleared := false
	target.script(func(_ context.Context, msg bus.Message, _ bus.Session) bus.Receipt {
		if msg.Held || msg.Status != "attempting" || msg.Receipt != nil {
			t.Fatalf("native transport received the held receipt: %+v", msg)
		}
		stored := readJSONFile(t, filepath.Join(service.Store.Dir, "messages", msg.ID+".json"))
		if _, present := stored["receipt"]; present {
			t.Fatal("native release did not clear the durable held receipt before transport")
		}
		observedCleared = true
		return model.NewReceipt(model.StatusSubmitted)
	})
	releaseModelHeld(t, service, to)
	stored := readJSONFile(t, filepath.Join(service.Store.Dir, "messages", held.ID+".json"))
	if _, present := stored["held"]; present {
		t.Fatal("released message kept its persisted held flag")
	}
	if !observedCleared || target.transportCalls() != 1 {
		t.Fatalf("native release: cleared=%v, transport=%d", observedCleared, target.transportCalls())
	}
}

func TestModelHookReleaseRebuildsPendingReceipt(t *testing.T) {
	service, target, to, held := newModelHeldFixture(t, true)
	releaseModelHeld(t, service, to)
	stored := readJSONFile(t, filepath.Join(service.Store.Dir, "messages", held.ID+".json"))
	if _, present := stored["held"]; present {
		t.Fatal("released message kept its persisted held flag")
	}
	if target.transportCalls() != 0 || stored["status"] != "pending_next_activity" {
		t.Fatalf("hook release: transport=%d, status=%v", target.transportCalls(), stored["status"])
	}
	receipt, ok := stored["receipt"].(map[string]any)
	if !ok {
		t.Fatal("hook release did not rebuild a receipt")
	}
	at, _ := receipt["at"].(string)
	requireModelTimestamp(t, at)
	receipt["at"] = "TIME"
	requireModelJSON(t, receipt, `{"at":"TIME","status":"pending_next_activity"}`)
}
