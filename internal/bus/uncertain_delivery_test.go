package bus

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// wordedAdapter is a client with its own wording for an unconfirmed delivery.
type wordedAdapter struct{ stubCodexAdapter }

func (wordedAdapter) Descriptor() agent.AdapterDescriptor {
	d := stubCodexAdapter{}.Descriptor()
	d.UncertainReceiver, d.UncertainAcknowledged = "receiver %s", "acknowledged %s"
	return d
}

func TestUncertainDeliveryComesFromTheAdapter(t *testing.T) {
	registry, err := agent.NewRegistry(wordedAdapter{}, stubClaudeAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: Store{dir: t.TempDir(), registry: registry}}
	if receiver, acknowledged := service.UncertainDelivery("codex"); receiver != "receiver %s" || acknowledged != "acknowledged %s" {
		t.Fatal(receiver, acknowledged)
	}
	// A client without wording and an unknown kind give nothing client-specific.
	for _, kind := range []string{"claude", "nobody", ""} {
		if receiver, acknowledged := service.UncertainDelivery(kind); receiver != "" || acknowledged != "" {
			t.Fatal(kind, receiver, acknowledged)
		}
	}
}
