package antigravity

import (
	"encoding/json"

	"github.com/zalkilabs/zalki/internal/agent"
)

// nativeFields are the Antigravity hook keys core keeps in HookInput.Native.
// Antigravity sends camelCase keys and no session_id: the conversation id is the
// session identity.
var nativeFields = []agent.NativeField{
	{Key: "conversationId", Kind: agent.NativeString, Capture: agent.CaptureID},
	{Key: "terminationReason", Kind: agent.NativeString},
}

// normalEnd is the terminationReason of a turn that ended because the model made
// no more tool calls. Every other reason (a cancel, an error, a limit) is an
// interrupt for Zalki.
const normalEnd = "NO_TOOL_CALL"

// nativeInput is what Antigravity adds to the common hook fields. Core has
// already checked the types.
type nativeInput struct {
	ConversationID    string `json:"conversationId"`
	TerminationReason string `json:"terminationReason"`
}

func nativeOf(event agent.HookInput) nativeInput {
	var native nativeInput
	_ = json.Unmarshal(event.Native, &native)
	return native
}
