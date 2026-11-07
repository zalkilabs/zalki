// Literal JSON expectations pin persisted fields independently of reflection.
package model

import (
	"encoding/json"
	"testing"
)

const messageZeroJSON = `{"schema":0,"id":"","from":"","to":"","body":"","target_generation":"","created_at":"","status":""}`

func TestMessageJSONOmissions(t *testing.T) {
	for _, test := range []struct {
		name  string
		value Message
		want  string
	}{
		{"zero", Message{}, messageZeroJSON},
		{"render only", Message{BusyDelivery: "next_turn", BodyPreview: "private preview"}, messageZeroJSON},
		{"receipt pointer", Message{Receipt: &Receipt{}}, `{"schema":0,"id":"","from":"","to":"","body":"","target_generation":"","created_at":"","status":"","receipt":{"at":"","status":""}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := json.Marshal(test.value)
			if err != nil || string(got) != test.want {
				t.Fatalf("message=%s, error=%v, want %s", got, err, test.want)
			}
		})
	}
}

func TestMessageFullJSON(t *testing.T) {
	value := Message{BusyDelivery: "next_hook", BodyPreview: "preview", Schema: 1, ID: "id", From: "alpha:from", Kind: "notice", FromName: "Sender", FromLabel: "Alpha", To: "beta:to", RequestedTo: "Alias", Body: "body", BodyOmitted: true, BodyDeferred: true, ReplyTo: "parent", TargetGeneration: "gen", CreatedAt: "created", Status: "submitted", Held: true, Receipt: &Receipt{At: "at", Status: "wake_hint_written", Code: "unavailable", Error: "error", Output: "output"}, AcknowledgedAt: "ack"}
	const want = `{"schema":1,"id":"id","from":"alpha:from","kind":"notice","from_name":"Sender","from_label":"Alpha","to":"beta:to","requested_to":"Alias","body":"body","body_omitted":true,"body_deferred":true,"reply_to":"parent","target_generation":"gen","created_at":"created","status":"submitted","held":true,"receipt":{"at":"at","status":"wake_hint_written","code":"unavailable","error":"error","output":"output"},"acknowledged_at":"ack"}`
	encoded, err := json.Marshal(value)
	if err != nil || string(encoded) != want {
		t.Fatalf("full message=%s, error=%v, want %s", encoded, err, want)
	}
	var decoded Message
	if err := json.Unmarshal([]byte(want), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.BusyDelivery != "" || decoded.BodyPreview != "" {
		t.Fatal("decoding restored render-only fields")
	}
	again, err := json.Marshal(decoded)
	if err != nil || string(again) != want {
		t.Fatalf("roundtrip=%s, error=%v, want %s", again, err, want)
	}
}

func TestMessageCopyKeepsReceiptPointer(t *testing.T) {
	original := Message{Receipt: &Receipt{Status: "submitted"}}
	copied := original
	if copied.Receipt != original.Receipt {
		t.Fatal("Message copy changes receipt pointer identity")
	}
	copied.Receipt.Output = "shared"
	if original.Receipt.Output != "shared" {
		t.Fatal("Message no longer has ordinary shallow-copy semantics")
	}
}
