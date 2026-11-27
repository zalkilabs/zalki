package antigravity

import (
	"errors"
	"io"
	"os"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// HandleHook delivers messages. Before a model call (PreInvocation, the common
// event PostToolUse) it writes pending messages as a system message and lets
// core mark the session busy. At a Stop it may continue the turn with them, and
// it ends the turn when the continue limit is reached.
func (Adapter) HandleHook(host agent.HookHost, event agent.HookInput, id string, output io.Writer) (bool, error) {
	switch event.Event {
	case "PostToolUse":
		return deliverBeforeModelCall(host, id, output)
	case "Stop", "Interrupt":
		lifecycle, ok := host.(agent.LifecycleHost)
		if !ok {
			return false, errors.New("Antigravity hook host lacks lifecycle support")
		}
		return endOfTurn(lifecycle, event, id, output)
	}
	return false, nil
}

// deliverBeforeModelCall writes the pending messages as a system message. Nothing
// pending means nothing is written, which Antigravity accepts. It returns false
// so that core applies the busy state.
func deliverBeforeModelCall(host agent.HookHost, id string, output io.Writer) (bool, error) {
	text, err := host.EmitPending(id)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil || text == "" {
		return false, err
	}
	return false, writeReply(output, injectStepsReply(text))
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
