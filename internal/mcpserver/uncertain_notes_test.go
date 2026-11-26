package mcpserver

// codexNotes stands in for bus.Service.UncertainDelivery with the sentences the
// Codex adapter declares today; every other kind has none.
func codexNotes(kind string) (receiver, acknowledged string) {
	if kind != "codex" {
		return "", ""
	}
	return "Message %s may already be in your Codex thread. If you can see it, ACK it; otherwise tell the sender delivery is still unconfirmed. Sending another copy may duplicate it.",
		"The receiver ACKed message %s, but Codex did not confirm the earlier delivery attempt."
}
