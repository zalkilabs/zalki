package codex

import "testing"

// Core shows these sentences for an unconfirmed Codex delivery; the wording is
// the Codex adapter's, not core's. Each has one %s for the message id.
func TestDescriptorDeclaresTheUncertainDeliveryWording(t *testing.T) {
	d := New(false, "fixture.json").Descriptor()
	if d.UncertainReceiver != "Message %s may already be in your Codex thread. If you can see it, ACK it; otherwise tell the sender delivery is still unconfirmed. Sending another copy may duplicate it." {
		t.Fatalf("UncertainReceiver = %q", d.UncertainReceiver)
	}
	if d.UncertainAcknowledged != "The receiver ACKed message %s, but Codex did not confirm the earlier delivery attempt." {
		t.Fatalf("UncertainAcknowledged = %q", d.UncertainAcknowledged)
	}
}
