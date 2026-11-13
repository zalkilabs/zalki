package pluginfeed

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// Generation replacement fences automatic delivery. Plugin parent/reply
// authority remains actor-based: a NEW reply to an admitted old parent targets
// the current registration under the same existing receive grant.
func TestPluginOldParentReplyTargetsPromptReplacement(t *testing.T) {
	x := newReplyFixture(t)
	if err := x.f.store.Update(x.from.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	if _, err := x.f.store.PruneRetired(false, nil, 0); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"conversation_id":%q,"cwd":"/workspace","hook_event_name":"beforeSubmitPrompt"}`, x.from.NativeID)
	if err := x.service.Hook(t.Context(), "cursor", strings.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	sessions, err := x.f.store.List()
	if err != nil {
		t.Fatal(err)
	}
	generation := ""
	for _, session := range sessions {
		if session.ID == x.from.ID {
			generation = session.Generation
		}
	}
	if generation == "" || generation == x.from.Generation {
		t.Fatal("C20_PROMPT_REPLACEMENT_FIXTURE_HAS_NEW_GENERATION")
	}
	reply, err := x.reply("new-reply-after-prompt", x.message.ID, x.from.ID)
	if err != nil || reply.ReplyTo != x.message.ID || reply.To != x.from.ID || reply.TargetGeneration != generation {
		t.Fatal("C20_NEW_PLUGIN_REPLY_TO_OLD_PARENT_USES_CURRENT_GENERATION", err, reply)
	}
}
