package cursor

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func TestCursorPromptNormalizationTrustAndIdentity(t *testing.T) {
	for _, event := range []string{"UserPromptSubmit", "SessionStart", "SessionEnd", "PostToolUse", "Stop", "Interrupt"} {
		input := agent.HookInput{SessionID: "other", ConversationID: "conversation", CWD: "/workspace", Event: event, AllowPromptRegistration: true}
		got, err := (Adapter{}).NormalizeHook(input)
		if err != nil || got.SessionID != "conversation" {
			t.Fatal("C02_CONVERSATION_ID_WINS", err)
		}
		if got.AllowPromptRegistration != (event == "UserPromptSubmit") {
			t.Fatal("C01_NORMALIZATION_ALONE_SELECTS_PROMPT_BOOTSTRAP", event)
		}
	}
}
