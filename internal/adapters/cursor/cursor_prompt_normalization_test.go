package cursor

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func TestCursorPromptNormalizationTrustAndIdentity(t *testing.T) {
	if !(Adapter{}).Descriptor().Lifecycle.PromptMayRegister {
		t.Fatal("C01_DESCRIPTOR_ALLOWS_PROMPT_BOOTSTRAP")
	}
	for _, event := range []string{"UserPromptSubmit", "SessionStart", "SessionEnd", "PostToolUse", "Stop", "Interrupt"} {
		input := withNative(agent.HookInput{SessionID: "other", CWD: "/workspace", Event: event, AllowPromptRegistration: true}, map[string]any{"conversation_id": "conversation"})
		got, err := (Adapter{}).NormalizeHook(input)
		if err != nil || got.SessionID != "conversation" {
			t.Fatal("C02_CONVERSATION_ID_WINS", err)
		}
	}
}
