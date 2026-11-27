package antigravity

import (
	"encoding/json"
	"io"
)

// Antigravity reads the stdout of a hook as one JSON object. These are the two
// replies Zalki writes; both shapes were accepted by agy 1.5.18 in a live run.
// Every other hook event writes nothing, which agy also accepts.

// permissionNote is the one sentence added to the Zalki instructions: agy denies
// a tool the user has not allowed, and only the user changes permissions.
const permissionNote = "If agy denies a Zalki tool for permission, tell the user to allow mcp(zalki/*) in agy's permission rules; do not change permissions yourself."

// injectStepsReply puts text into the conversation as a system message. The
// client keeps it in the conversation, also when the chat is resumed.
func injectStepsReply(text string) any {
	return map[string]any{"injectSteps": []any{map[string]any{"systemMessage": map[string]string{"systemMessage": text}}}}
}

// continueReply continues a turn that is about to end; reason is shown to the
// model as a system message.
func continueReply(text string) any {
	return map[string]string{"decision": "continue", "reason": text}
}

// writeReply writes one reply and a newline.
func writeReply(output io.Writer, reply any) error {
	raw, err := json.Marshal(reply)
	if err != nil {
		return err
	}
	_, err = output.Write(append(raw, '\n'))
	return err
}
