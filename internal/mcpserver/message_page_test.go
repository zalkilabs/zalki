package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestMessagePageBoundsResponsesWithoutTruncatingBodies(t *testing.T) {
	messages := []bus.Message{
		{ID: "00000000-0000-4000-8000-000000000001", Body: strings.Repeat("a", 16000)},
		{ID: "00000000-0000-4000-8000-000000000002", Body: strings.Repeat("b", 16000)},
	}
	page := messagePage(bus.MessageBatch{Messages: messages}, false)
	encoded, _ := json.Marshal(page)
	if len(encoded) > messagePageBytes || len(page.Messages) != 1 || !page.More || page.Messages[0].Body != messages[0].Body {
		t.Fatal("page was unbounded, lost continuation, or truncated a body")
	}
	messages[0].Body = strings.Repeat("\x01", 16000)
	page = messagePage(bus.MessageBatch{Messages: messages[:1]}, false)
	if len(page.Messages) != 1 || !page.Messages[0].BodyOmitted || page.Messages[0].Body != "" {
		t.Fatal("escape-heavy message did not retain an exact-read ID")
	}
	page = messagePage(bus.MessageBatch{Messages: messages[:1]}, true)
	if page.Messages[0].Body != messages[0].Body || page.Messages[0].BodyOmitted {
		t.Fatal("explicit exact lookup truncated content")
	}
}
