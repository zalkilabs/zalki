package cursor

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestListenStopsAtAcquisitionFailure(t *testing.T) {
	want := errors.New("lease refused")
	acquire := func(actor, key string) (ListenerHandle, error) {
		if actor != "test-actor" || key != "test-capability" {
			t.Fatalf("acquisition received different credentials")
		}
		// This conversion produces a non-nil interface containing a nil
		// pointer. An acquisition error must be handled before using it.
		var lease *bus.Lease
		return lease, want
	}
	inspect := func(string) (*model.Endpoint, error) {
		t.Fatal("endpoint inspection ran after acquisition failed")
		return nil, nil
	}
	if err := Listen(context.Background(), acquire, inspect, "test-actor", "test-capability", io.Discard); !errors.Is(err, want) {
		t.Fatalf("acquisition cause was lost: %v", err)
	}
}
