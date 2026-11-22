package mcpserver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

// Pin the full advertised tools/list contract, including discovery's optional
// skipped_sessions count. Guard content as well as size during refactors.
// Exclude the result envelope: its serverInfo version contains a dynamic build ID.
func TestAdvertisedToolContractUnchanged(t *testing.T) {
	client, ctx := testClient(t, &bus.Service{})
	result, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Only the explicit plugin destination documentation changes. Restore that
	// one value, then compare the FULL typed catalog to the released digest.
	descriptions := 0
	for _, tool := range result.Tools {
		if tool.Name != "send_message" {
			continue
		}
		schema, ok := tool.InputSchema.(map[string]any)
		if !ok {
			t.Fatal("send input schema changed")
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatal("send properties changed")
		}
		to, ok := properties["to"].(map[string]any)
		if !ok || to["description"] != "ID/name; plugin:UUID stores, no delivery" {
			t.Fatal("plugin storage description missing")
		}
		to["description"] = "ID or unique name; optionally kind:name."
		descriptions++
	}
	if descriptions != 1 {
		t.Fatal("send tool missing or duplicated")
	}
	encoded, err := json.Marshal(result.Tools)
	if err != nil {
		t.Fatal(err)
	}
	// Only list_messages' output schema gains the optional unreadable
	// integer (+32 schema bytes, like skipped_sessions); no description changed.
	// Later, the send, reply and ack output schema gains state, to, to_name and
	// parent_acknowledged, and the list_messages message view gains state and to_name.
	// Then the descriptions name the required keys (actor, capability, to,
	// message_id) and the credentials come from the newest SessionStart context.
	// Last, list_messages says who owns a deferred native body and how to read
	// a shortened one.
	const want = "fd0400e172676b19e6e7a77cbcc0c3bd9772d3130d82d5c018f8db766f8f568d"
	if got := fmt.Sprintf("%x", sha256.Sum256(encoded)); got != want {
		t.Fatalf("advertised tool contract changed: sha256=%s bytes=%d; compare with the pre-refactor contract before updating", got, len(encoded))
	}
}
