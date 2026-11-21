package cursor

import (
	"encoding/json"

	"github.com/zalkilabs/zalki/internal/agent"
)

// nativeFields are the Cursor hook keys core keeps in HookInput.Native.
var nativeFields = []agent.NativeField{
	{Key: "conversation_id", Kind: agent.NativeString, Capture: agent.CaptureID},
	{Key: "workspace_roots", Kind: agent.NativeStrings, Capture: agent.CapturePaths},
	{Key: "status", Kind: agent.NativeString},
	{Key: "loop_count", Kind: agent.NativeInt},
}

// nativeInput is what Cursor adds to the common hook fields. Core has already
// checked the types, so a decode error cannot happen for hook input from core.
type nativeInput struct {
	ConversationID string   `json:"conversation_id"`
	WorkspaceRoots []string `json:"workspace_roots"`
	Status         string   `json:"status"`
	LoopCount      int      `json:"loop_count"`
}

func nativeOf(event agent.HookInput) nativeInput {
	var native nativeInput
	_ = json.Unmarshal(event.Native, &native)
	return native
}
