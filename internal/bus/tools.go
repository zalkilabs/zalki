package bus

type ToolArgs struct {
	Actor       string `json:"actor"`
	Capability  string `json:"capability"`
	To          string `json:"to"`
	MessageID   string `json:"message_id"`
	Body        string `json:"body"`
	History     bool   `json:"history,omitempty"`
	Sent        bool   `json:"sent,omitempty"`
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
			"actor":      stringField("SessionStart actor."),
			"capability": stringField("SessionStart capability."),
		}
	}
	send := identity()
	send["to"] = stringField("ID or unique name; optionally kind:name.")
	send["message_id"] = stringField("Optional UUID; supply before sending for retry safety.")
	send["body"] = stringField("Message text, 1..16384 UTF-8 bytes.")
	ack := identity()
	ack["message_id"] = stringField("Incoming message UUID you received.")
	reply := identity()
	reply["message_id"] = stringField("Incoming message ID; outgoing ID is generated.")
	reply["body"] = send["body"]
	inbox := identity()
	inbox["cursor"] = stringField("Use next_cursor to continue a page with the same filters.")
	inbox["message_id"] = stringField("Optional exact message/receipt UUID; avoids an inbox scan.")
	inbox["include_body"] = map[string]any{"type": "boolean", "description": "Include history bodies; default summaries only."}
	inbox["history"] = map[string]any{"type": "boolean", "description": "All sent/received history; default unread inbox."}
	inbox["sent"] = map[string]any{"type": "boolean", "description": "Sent-only history and receipts, newest first."}
	inbox["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum records; default 5."}
	watch := identity()
	watch["to"] = stringField("ID or unique name.")
	return []map[string]any{
		{"name": "list_agents", "description": "Discover agents; registration does not prove a live terminal. Omitted state=ready, liveness=unknown, experimental=false; kind is ID prefix. delivery is busy timing; idle wake unless next_hook (idle_wake overrides). updated_at is last activity in UTC seconds, not a heartbeat.", "inputSchema": schema(map[string]any{}), "annotations": map[string]bool{"readOnlyHint": true}},
		{"name": "send_message", "description": "Send a new agent message; never auto-resend.", "inputSchema": schema(send, "actor", "capability", "to", "body")},
		{"name": "reply_message", "description": "Acknowledge and reply to the exact sender. Same ID/body never resends; ACK persists if delivery fails.", "inputSchema": schema(reply, "actor", "capability", "message_id", "body")},
		{"name": "ack_message", "description": "Record receipt locally without sending a message.", "inputSchema": schema(ack, "actor", "capability", "message_id")},
		{"name": "list_messages", "description": "Unread inbox, sent:true history, or exact message_id. History omits bodies. Page with next_cursor. Fetch body_omitted by ID; await body_deferred delivery.", "inputSchema": schema(inbox, "actor", "capability"), "annotations": map[string]bool{"readOnlyHint": true}},
		{"name": "watch_agent", "description": "One notice when an agent next finishes or ends; expires in 12h.", "inputSchema": schema(watch, "actor", "capability", "to")},
	}
}
