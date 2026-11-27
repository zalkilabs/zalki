package antigravity

import (
	"errors"
	"io"
	"os"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// HandleHook delivers messages. Before a model call (PreInvocation, the common
// event Activity) it revives a retired chat, hands a chat that core just
// registered its credentials, and writes pending messages as a system message.
// At a Stop it may continue the turn with them, and it ends the turn when the
// continue limit is reached.
func (Adapter) HandleHook(host agent.HookHost, event agent.HookInput, id string, output io.Writer) (bool, error) {
	switch event.Event {
	case "Activity":
		return beforeModelCall(host, event, id, output)
	case "Stop", "Interrupt":
		lifecycle, ok := host.(agent.LifecycleHost)
		if !ok {
			return false, errors.New("Antigravity hook host lacks lifecycle support")
		}
		return endOfTurn(lifecycle, event, id, output)
	}
	return false, nil
}

// beforeModelCall runs at every model call, in this order: the chat is marked busy
// (and revived if it was retired: its credentials are kept, see core), a chat that
// core registered with this very hook gets new credentials, and pending messages
// are written. Credentials and messages go out in one system message. A failure
// after the credentials were claimed still sends them, so they are never lost.
func beforeModelCall(host agent.HookHost, event agent.HookInput, id string, output io.Writer) (bool, error) {
	lifecycle, ok := host.(agent.LifecycleHost)
	if !ok {
		return false, errors.New("Antigravity hook host lacks lifecycle support")
	}
	var parts []string
	if event.Registered {
		grant, err := lifecycle.ClaimRecovery(claimNewChat)
		if err != nil {
			return true, err
		}
		if grant.Capability != "" {
			parts = append(parts, grant.Instructions+" "+permissionNote)
		}
	}
	if err := lifecycle.ApplyLifecycle(reviveChat); err != nil {
		return true, errors.Join(err, writeParts(output, parts))
	}
	text, err := host.EmitPending(id)
	if errors.Is(err, os.ErrNotExist) {
		return true, writeParts(output, parts)
	}
	if text != "" {
		parts = append(parts, text)
	}
	return true, errors.Join(err, writeParts(output, parts))
}

// writeParts writes the parts as one system message; nothing is written for no parts.
func writeParts(output io.Writer, parts []string) error {
	if len(parts) == 0 {
		return nil
	}
	return writeReply(output, injectStepsReply(strings.Join(parts, "\n")))
}

// claimNewChat asks for credentials for a chat that core registered at this hook.
func claimNewChat(model.SessionView) (agent.RecoveryDecision, error) {
	return agent.RecoveryDecision{Recover: true}, nil
}

// reviveChat marks the chat busy and, when it was retired, brings it back with
// its counter reset. Its capability was kept through the retirement.
func reviveChat(v model.SessionView) (agent.LifecycleChange, error) {
	change := agent.LifecycleChange{Transition: agent.TransitionPromptReactivate}
	if v.Retired {
		change.AdapterState = encodeState(loopState{})
	}
	return change, nil
}

// endOfTurn marks the session ready. A normal end with pending messages and room
// under the continue limit continues the turn with them; anything else ends it
// and resets the count. The count is written before a message is marked as
// emitted, so a failed write never loses a message: the first write already
// counts the continue (the next count, or zero when the limit is reached), and a
// Stop with nothing pending resets it.
func endOfTurn(host agent.LifecycleHost, event agent.HookInput, id string, output io.Writer) (bool, error) {
	if event.Event == "Interrupt" {
		return true, host.ApplyLifecycle(readyWithContinues(0))
	}
	var refused bool
	if err := host.ApplyLifecycle(func(v model.SessionView) (agent.LifecycleChange, error) {
		count := decodeState(v.AdapterState).Continues
		refused = count >= maxContinues
		if refused {
			return readyWithContinues(0)(v)
		}
		return readyWithContinues(count + 1)(v)
	}); err != nil {
		return true, err
	}
	if refused {
		return true, nil
	}
	text, err := host.EmitPending(id)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	if text == "" {
		return true, host.ApplyLifecycle(readyWithContinues(0))
	}
	return true, writeReply(output, continueReply(text))
}

// readyWithContinues marks the session ready and stores the continue count.
func readyWithContinues(count int) func(model.SessionView) (agent.LifecycleChange, error) {
	return func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{Transition: agent.TransitionReady, AdapterState: encodeState(loopState{Continues: count})}, nil
	}
}
