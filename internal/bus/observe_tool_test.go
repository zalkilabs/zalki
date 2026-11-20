package bus

import (
	"errors"
	"testing"
)

// ObserveTool marks the actor on the runtime lease after a successful tool
// call, and only then. The MCP server used to do this itself.
func TestObserveToolMarksActorOnlyAfterSuccess(t *testing.T) {
	store := fixtureStore(t)
	ok, _ := register(t, store, "cursor")
	failed, _ := register(t, store, "cursor")
	lease, err := store.TrackRuntime("mcp", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	service := &Service{Store: store, Runtime: lease}

	service.ObserveTool("send_message", failed.ID, "", 1, errors.New("failed"))
	if got := lease.Info().Actors; len(got) != 0 {
		t.Fatalf("a failed call marked actors %v", got)
	}
	service.ObserveTool("send_message", ok.ID, "", 1, nil)
	if got := lease.Info().Actors; len(got) != 1 || got[0] != ok.ID {
		t.Fatalf("a successful call marked actors %v, want [%s]", got, ok.ID)
	}
}

func TestObserveToolWithoutRuntimeLease(t *testing.T) {
	service := &Service{Store: fixtureStore(t)}
	service.ObserveTool("send_message", "cursor:x", "", 1, nil) // must not panic
}

func TestServiceStorePassThroughs(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "cursor")
	service := &Service{Store: store}
	rows, _, err := service.DiscoverWithWarnings(from.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	direct, _, _ := store.DiscoverWithWarnings(from.ID, key)
	if len(rows) != len(direct) || len(rows) == 0 {
		t.Fatalf("discover %d rows, store %d", len(rows), len(direct))
	}
	batch, err := service.ReadMessages(MessageQuery{Actor: from.ID, Capability: key})
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Messages) != 0 {
		t.Fatalf("empty inbox returned %d messages", len(batch.Messages))
	}
	if _, err := service.Ack(from.ID, key, newID()); err == nil {
		t.Fatal("ack of an unknown message succeeded")
	}
}
