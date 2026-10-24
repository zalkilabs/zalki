package bus

type ToolArgs struct {
	Actor       string `json:"actor"`
	Capability  string `json:"capability"`
	To          string `json:"to"`
	MessageID   string `json:"message_id"`
	Body        string `json:"body"`
	History     bool   `json:"history,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	IncludeBody bool   `json:"include_body,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
}

func schema(properties map[string]any, required ...string) map[string]any {
	v := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) > 0 {
		v["required"] = required
	}
	return v
}

func stringField(description string) map[string]string {
	return map[string]string{"type": "string", "description": description}
}

func Tools() []map[string]any {
	identity := func() map[string]any {
		return map[string]any{
			"actor":      stringField("Your SessionStart actor."),
			"capability": stringField("Your private SessionStart capability."),
		}
	}
	send := identity()
	send["to"] = stringField("Exact ID or unique name from list_agents; optionally kind:name.")
	send["message_id"] = stringField("New UUID; reuse on retry without resending.")
	send["body"] = stringField("Message text, 1..16384 UTF-8 bytes.")
	ack := identity()
	ack["message_id"] = stringField("Incoming message UUID you received.")
	reply := identity()
	reply["message_id"] = ack["message_id"]
	reply["body"] = send["body"]
	inbox := identity()
	inbox["cursor"] = stringField("Use next_cursor to continue a page with the same filters.")
	inbox["message_id"] = stringField("Optional exact message/receipt UUID; avoids an inbox scan.")
	inbox["include_body"] = map[string]any{"type": "boolean", "description": "Include history bodies; default summaries only."}
	inbox["history"] = map[string]any{"type": "boolean", "description": "Include sent/acknowledged history; default unread inbox."}
	inbox["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum records; default 5."}
	return []map[string]any{
		{"name": "list_agents", "description": "Discover names and receive capabilities; registration does not prove a live terminal.", "inputSchema": schema(map[string]any{}), "annotations": map[string]bool{"readOnlyHint": true}},
		{"name": "send_message", "description": "Send a new agent message; never auto-resend.", "inputSchema": schema(send, "actor", "capability", "to", "message_id", "body")},
		{"name": "reply_message", "description": "Acknowledge and reply to the exact sender. Same ID/body never resends; ACK persists if delivery fails.", "inputSchema": schema(reply, "actor", "capability", "message_id", "body")},
		{"name": "ack_message", "description": "Record receipt locally without sending a message.", "inputSchema": schema(ack, "actor", "capability", "message_id")},
		{"name": "list_messages", "description": "Read unread inbox or one message_id. History is opt-in and body-free by default. Continue more=true pages with next_cursor. Fetch body_omitted by ID; never act on body_deferred records.", "inputSchema": schema(inbox, "actor", "capability"), "annotations": map[string]bool{"readOnlyHint": true}},
	}
}
