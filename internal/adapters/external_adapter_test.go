package adapters_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

// fourthAdapter is compiled outside package bus. It proves an external client
// can implement the exported adapter contract, including unexported optional
// interfaces, and be composed with the built-in registry.
type fourthAdapter struct{ bus.StandardHooks }

func (fourthAdapter) Descriptor() bus.AdapterDescriptor {
	return bus.AdapterDescriptor{
		Kind: "fourth", Label: "Fourth", Hooks: bus.StandardEvents(true),
		ProcessNames: []string{"fourth"}, OwnerRole: "client", Executable: "fourth",
	}
}

func (fourthAdapter) ReceiveCapabilities(bus.Session) bus.ReceiveCapabilities {
	return bus.ReceiveCapabilities{
		Delivery: "external_log", Inbox: bus.InboxNative, BusyDelivery: bus.DeliveryNextTurn, LifecycleHooks: true,
	}
}

func (fourthAdapter) Deliver(context.Context, bus.Message, bus.Session) bus.Receipt {
	receipt := bus.NewReceipt(bus.StatusSubmitted)
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
	registry, err := bus.NewRegistry(codex, claude, bus.CursorAdapter(), fourthAdapter{})
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
	if err != nil || msg.Status != bus.StatusSubmitted || msg.Receipt == nil || msg.Receipt.Output != "external-fourth" {
		t.Fatalf("external delivery was not accepted: %+v %v", msg, err)
	}
	files, err := registry.Configuration("/bin/zalki", store.Dir)
	if err != nil || len(files) == 0 {
		t.Fatal("built-in configuration disappeared beside the external adapter", err)
	}
}
