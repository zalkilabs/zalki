// Package antigravity is the adapter for Antigravity (the agy command). Zalki
// installs three hooks: SessionStart, which
// registers the conversation and gives it its credentials, PreInvocation, which
// runs before each model call (it registers a resumed chat that sent no start
// hook, revives a retired one and delivers messages), and Stop, which can
// continue a turn that is about to end. Antigravity has no way to wake an idle chat, so a
// message to an idle chat waits for its next hook.
package antigravity

import (
	"context"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// Adapter implements the Antigravity client.
type Adapter struct{ agent.StandardHooks }

var (
	_ agent.HookAdapter         = Adapter{}
	_ agent.SessionStateAdapter = Adapter{}
	_ agent.HealthAdapter       = Adapter{}
)

// New returns the Antigravity adapter.
func New() Adapter { return Adapter{} }

func (Adapter) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{
		Kind: "antigravity", Label: "Antigravity", HookEventKey: "hookEventName",
		// PreInvocation fires before every model call, also in a resumed chat that sent
		// no start hook. It is the Activity event, which may register the chat.
		Hooks:        map[string]string{"SessionStart": "SessionStart", "PreInvocation": "Activity", "Stop": "Stop"},
		Lifecycle:    agent.Lifecycle{ActivityMayRegister: true},
		Native:       nativeFields,
		ProcessNames: []string{"agy"}, OwnerRole: "client", Executable: "agy",
		InstallNotice: "Antigravity: restart agy to load the Zalki hooks. To let agy chats use the Zalki tools without asking, allow mcp(zalki/*) in agy's permission rules; Zalki does not change permissions.",
		Notes: []string{
			"Antigravity cannot wake an idle chat. A message reaches a running chat before its next model call or when its turn ends, and an idle chat gets it at its next hook.",
			"A chat that was resumed with agy --resume sends no start hook. Zalki registers it at its first model call and gives it new credentials if it had none.",
		},
	}
}

func (Adapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{
		Delivery: "next_invocation_or_stop_hook", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook,
		LifecycleHooks: true, HookDelivery: true,
	}
}

// Deliver stores the message for the next hook: there is no live path to a chat.
func (Adapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusPendingActivity)
}

// Health reports that messages are delivered by hooks only.
func (Adapter) Health(agent.HealthHost, model.Session) (string, string) { return "deferred", "" }

// StartupOutput gives the new conversation its Zalki instructions as a system message.
func (Adapter) StartupOutput(startup agent.StartupContext) (any, error) {
	return injectStepsReply(startup.Instructions + " " + permissionNote), nil
}

// NormalizeHook takes the session id from the conversation id (Antigravity sends
// no session_id) and turns a turn that did not end normally into an interrupt.
func (Adapter) NormalizeHook(event agent.HookInput) (agent.HookInput, error) {
	native := nativeOf(event)
	event.SessionID = native.ConversationID
	if event.Event == "Stop" && native.TerminationReason != "" && native.TerminationReason != normalEnd {
		event.Event = "Interrupt"
	}
	return event, nil
}
