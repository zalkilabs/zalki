package cursor

import (
	"fmt"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/install"
)

func cursorListenerCommand(binary, actor, capability, state, configFile string) string {
	args := []string{binary, "--config", configFile, "--state", state, "cursor-listen", "--actor", actor}
	for i := range args {
		args[i] = install.ShellQuote(args[i])
	}
	return "ZALKI_CAPABILITY=" + install.ShellQuote(capability) + " " + strings.Join(args, " ")
}

func cursorRecoveryContext(binary string, claim agent.RecoveryGrant) string {
	if claim.Capability == "" {
		return ""
	}
	command := cursorListenerCommand(binary, claim.Actor, claim.Capability, claim.StateDir, claim.ConfigFile)
	return fmt.Sprintf("[Zalki local listener recovery]\nZalki registered this resumed chat. Set up its listener using the private instructions below.\n%s\nYour registered Cursor session has no verified live listener. "+
		"Your actor remains %s. Your refreshed private capability is %s; use it for future zalki calls. "+
		"This replaces only your own previous capability; never reveal it or borrow another session's. "+
		"If a listener setup command was already denied, do not retry it: report that and keep deferred hook delivery. "+
		"Otherwise, under your normal permissions, arm one native Shell background task with block_until_ms=0 and "+
		"notify_on_output={pattern:'ZALKI_WAKE ',reason:'zalki message arrived'} running: `%s`. "+
		"If your native task registry shows an old unusable cursor-listen task for this exact actor, cancel only that task before replacing it. "+
		"Do not inspect terminal logs, read credential files, poll, wait for output, change permissions, or start another listener after a refusal. "+
		"If setup is unavailable or denied, report it and retain deferred delivery. End your turn normally. "+
		"On ZALKI_WAKE, read list_messages with that message_id; use reply_message to acknowledge and reply, or ack_message when no reply is needed. "+
		"This setup instruction is emitted once for this missing-listener interval; it is not an agent request.", claim.Instructions, claim.Actor, claim.Capability, command)
}
