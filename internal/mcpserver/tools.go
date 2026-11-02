package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
)

type toolArgs struct {
	Actor       string `json:"actor"`
	Capability  string `json:"capability"`
	To          string `json:"to"`
	MessageID   string `json:"message_id"`
	Body        string `json:"body"`
	History     bool   `json:"history,omitempty"`
	Sent        bool   `json:"sent,omitempty"`
	Preview     bool   `json:"preview,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	IncludeBody bool   `json:"include_body,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
	Task        string `json:"task,omitempty"`
}

// Each declaration owns its advertised contract, argument conveniences, and
// typed handler. Only this table registers tools and supplies middleware rules.
type toolDefinition struct {
	name, description string
	input             inputSchema
	readOnly          bool
	aliases           []argumentAlias
	hint              string
	validate          func(map[string]json.RawMessage) error
}

type toolSpec struct {
	toolDefinition
	register func(*mcp.Server, *bus.Service)
}

type inputSchema struct {
	Type                 string         `json:"type"`
	Properties           map[string]any `json:"properties"`
	AdditionalProperties bool           `json:"additionalProperties"`
	Required             []string       `json:"required,omitempty"`
}

// Preserve the original schema's canonical map-key order on the wire.
func (s inputSchema) MarshalJSON() ([]byte, error) {
	value := map[string]any{"type": s.Type, "properties": s.Properties, "additionalProperties": s.AdditionalProperties}
	if len(s.Required) > 0 {
		value["required"] = s.Required
	}
	return json.Marshal(value)
}

func schema(properties map[string]any, required ...string) inputSchema {
	return inputSchema{Type: "object", Properties: properties, Required: required}
}

func defineTool[T any](definition toolDefinition, call func(context.Context, *bus.Service, toolArgs) (T, error)) toolSpec {
	return toolSpec{toolDefinition: definition, register: func(server *mcp.Server, service *bus.Service) {
		tool := &mcp.Tool{Name: definition.name, Description: definition.description, InputSchema: definition.input}
		if definition.readOnly {
			tool.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
		}
		// Clients require concrete output schemas; interface{} produces a boolean schema.
		addTool(server, service, tool, func(ctx context.Context, args toolArgs) (T, error) {
			return call(ctx, service, args)
		})
	}}
}

func stringField(description string) map[string]string {
	return map[string]string{"type": "string", "description": description}
}

func toolTable() []toolSpec {
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
	inbox["to"] = stringField("Sent recipient ID or current unique name; requires sent:true.")
	inbox["preview"] = map[string]any{"type": "boolean", "description": "Sent preview: one line, ≤80 UTF-8 bytes; requires sent:true."}
	inbox["limit"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100, "description": "Maximum records; default 5."}
	watch := identity()
	watch["to"] = stringField("ID or unique name.")
	task := identity()
	task["task"] = stringField("One line, ≤120 bytes. Empty clears.")
	return []toolSpec{
		defineTool(toolDefinition{
			name:        "list_agents",
			description: "Discover agents; registration does not prove a live terminal. Omitted state=ready, liveness=unknown, experimental=false; kind is ID prefix. delivery is busy timing; idle wake unless next_hook (idle_wake overrides). updated_at is last activity in UTC seconds, not a heartbeat. task: self-claimed. Optional actor+capability marks self; both must authenticate.",
			input:       schema(identity()), readOnly: true,
			hint: " Omit credentials or provide actor and capability together to mark self.",
			validate: func(arguments map[string]json.RawMessage) error {
				_, actorGiven := arguments["actor"]
				_, keyGiven := arguments["capability"]
				if actorGiven || keyGiven {
					var actor, key string
					_ = json.Unmarshal(arguments["actor"], &actor)
					_ = json.Unmarshal(arguments["capability"], &key)
					if actor == "" || key == "" {
						return fmt.Errorf("actor and capability must both be nonempty when provided")
					}
				}
				return nil
			},
		}, func(_ context.Context, service *bus.Service, args toolArgs) ([]bus.AgentInfo, error) {
			return service.Store.DiscoverFor(args.Actor, args.Capability)
		}),
		defineTool(toolDefinition{
			name: "send_message", description: "Send a new agent message; never auto-resend. message aliases body.",
			input:   schema(send, "actor", "capability", "to", "body"),
			aliases: []argumentAlias{{"message", "body"}},
			hint:    " Use body for message text. message_id is optional; pass it to make retries safe.",
		}, func(ctx context.Context, service *bus.Service, args toolArgs) (MessageReceipt, error) {
			return messageReceipt(service.Send(ctx, bus.SendRequest{
				Actor: args.Actor, Capability: args.Capability,
				To: args.To, MessageID: args.MessageID, Body: args.Body,
			}))
		}),
		defineTool(toolDefinition{
			name: "reply_message", description: "Acknowledge and reply to the exact sender. Same ID/body never resends; ACK persists if delivery fails. message aliases body; reply_to aliases message_id.",
			input:   schema(reply, "actor", "capability", "message_id", "body"),
			aliases: []argumentAlias{{"message", "body"}, {"reply_to", "message_id"}},
			hint:    " message_id is the incoming message you are replying to; body is the reply. The outgoing ID is generated.",
		}, func(ctx context.Context, service *bus.Service, args toolArgs) (MessageReceipt, error) {
			return messageReceipt(service.Reply(ctx, bus.ReplyRequest{
				Actor: args.Actor, Capability: args.Capability,
				MessageID: args.MessageID, Body: args.Body,
			}))
		}),
		defineTool(toolDefinition{
			name: "ack_message", description: "Record receipt locally without sending a message.",
			input: schema(ack, "actor", "capability", "message_id"),
		}, func(_ context.Context, service *bus.Service, args toolArgs) (MessageReceipt, error) {
			return messageReceipt(service.Store.Ack(args.Actor, args.Capability, args.MessageID))
		}),
		defineTool(toolDefinition{
			name: "list_messages", description: "Unread inbox, sent:true history, or exact message_id. History omits bodies. Page with next_cursor. Fetch body_omitted by ID; await body_deferred delivery.",
			input: schema(inbox, "actor", "capability"), readOnly: true,
		}, func(_ context.Context, service *bus.Service, args toolArgs) (MessagePage, error) {
			messages, err := service.Store.ReadMessages(bus.MessageQuery{
				Actor: args.Actor, Capability: args.Capability,
				History: args.History, Limit: args.Limit,
				Sent: args.Sent, To: args.To, Preview: args.Preview,
				MessageID: args.MessageID, IncludeBody: args.IncludeBody, Cursor: args.Cursor,
			})
			return messagePage(messages, args.MessageID != ""), err
		}),
		defineTool(toolDefinition{
			name: "watch_agent", description: "One notice when an agent next finishes or ends; expires in 12h.",
			input: schema(watch, "actor", "capability", "to"),
		}, func(_ context.Context, service *bus.Service, args toolArgs) (bus.WatchResult, error) {
			return service.Watch(args.Actor, args.Capability, args.To)
		}),
		defineTool(toolDefinition{
			name: "set_task", description: "Set your own one-line task. Empty clears. Unverified, omitted when empty, and not an address.",
			input: schema(task, "actor", "capability", "task"),
		}, func(_ context.Context, service *bus.Service, args toolArgs) (bus.TaskResult, error) {
			return service.SetTask(args.Actor, args.Capability, args.Task)
		}),
	}
}
