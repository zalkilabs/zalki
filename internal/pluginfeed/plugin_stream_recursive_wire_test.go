package pluginfeed

import (
	"encoding/json"
	"testing"
)

func TestStreamRecursiveWireWhitelistRejectsPrivateData(t *testing.T) {
	feed, _, _, _, _ := streamPendingMessageFixture(t)
	event := readStreamCheckpoint(t, feed).Pending.Frames[0]
	for _, tc := range []struct {
		name string
		raw  []byte
		path []string
		key  string
	}{
		{"root-policy", event, nil, "plugin_policy"},
		{"event-owner", event, []string{"event"}, "owner"},
		{"message-body", event, []string{"event", "message"}, "body"},
		{"message-preview", event, []string{"event", "message"}, "preview"},
		{"message-size", event, []string{"event", "message"}, "body_size"},
		{"message-receipt", event, []string{"event", "message"}, "receipt"},
		{"message-binding", event, []string{"event", "message"}, "plugin_binding"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var root map[string]any
			if json.Unmarshal(tc.raw, &root) != nil {
				t.Fatal("valid fixture")
			}
			object := root
			for _, key := range tc.path {
				object = object[key].(map[string]any)
			}
			object[tc.key] = "PRIVATE_WIRE_SENTINEL"
			raw, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			if validateStreamFrame(raw) == nil {
				t.Fatal("F_WIRE_RECURSIVE_PRIVATE_FIELD_REFUSED", tc.name)
			}
		})
	}
}
