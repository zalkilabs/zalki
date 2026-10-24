// Package mcpserver handles MCP over stdio.
package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
)

type Output[T any] struct {
	Data    T                  `json:"data"`
	Runtime *bus.RuntimeStatus `json:"runtime,omitempty"`
	Error   *ToolError         `json:"error,omitempty"`
}

type ToolError struct {
	Code    bus.ErrorCode `json:"code"`
	Message string        `json:"message"`
}

// Mutations return receipts; full bodies and routing records live in history.
type MessageReceipt struct {
	ID           string             `json:"id"`
	Status       bus.DeliveryStatus `json:"status"`
	Acknowledged bool               `json:"acknowledged"`
	Error        string             `json:"error,omitempty"`
	Code         bus.ErrorCode      `json:"code,omitempty"`
}

func messageReceipt(message bus.Message, err error) (MessageReceipt, error) {
	receipt := MessageReceipt{ID: message.ID, Status: message.Status, Acknowledged: message.AcknowledgedAt != ""}
	if message.Receipt != nil {
		receipt.Error = message.Receipt.Error
		receipt.Code = message.Receipt.Code
	}
	return receipt, err
}

// Healthy builds are identified at MCP initialization; only actionable runtime
// warnings belong in every tool result. Full diagnostics remain in doctor.
func runtimeWarning(service *bus.Service) *bus.RuntimeStatus {
	status := service.RuntimeStatus()
	if !status.NeedsRestart && status.BuildError == "" {
		return nil
	}
	return &status
}

// MessageView omits journal internals and native transport output.
type MessageView struct {
	ID             string             `json:"id"`
	From           string             `json:"from"`
	FromName       string             `json:"from_name,omitempty"`
	FromLabel      string             `json:"from_label,omitempty"`
	To             string             `json:"to"`
	Body           string             `json:"body,omitempty"`
	BodyOmitted    bool               `json:"body_omitted,omitempty"`
	BodyDeferred   bool               `json:"body_deferred,omitempty"`
	ReplyTo        string             `json:"reply_to,omitempty"`
	CreatedAt      string             `json:"created_at"`
	Status         bus.DeliveryStatus `json:"status"`
	AcknowledgedAt string             `json:"acknowledged_at,omitempty"`
	Error          string             `json:"error,omitempty"`
	Code           bus.ErrorCode      `json:"code,omitempty"`
}

func messageViews(messages []bus.Message) []MessageView {
	views := make([]MessageView, 0, len(messages))
	for _, msg := range messages {
		view := MessageView{ID: msg.ID, From: msg.From, FromName: msg.FromName, FromLabel: msg.FromLabel,
			To: msg.To, Body: msg.Body, BodyDeferred: msg.BodyDeferred, BodyOmitted: msg.BodyOmitted, ReplyTo: msg.ReplyTo,
			CreatedAt: msg.CreatedAt, Status: msg.Status, AcknowledgedAt: msg.AcknowledgedAt}
		if msg.Receipt != nil {
			view.Error, view.Code = msg.Receipt.Error, msg.Receipt.Code
		}
		views = append(views, view)
	}
	return views
}

const messagePageBytes = 24 * 1024

type MessagePage struct {
	Messages   []MessageView `json:"messages"`
	More       bool          `json:"more"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

func messagePage(batch bus.MessageBatch, exact bool) MessagePage {
	page := MessagePage{Messages: []MessageView{}, More: batch.More}
	used := len(`{"messages":[],"more":false,"next_cursor":"00000000-0000-4000-8000-000000000000"}`)
	for _, view := range messageViews(batch.Messages) {
		encoded, _ := json.Marshal(view)
		if !exact && used+len(encoded)+1 > messagePageBytes {
			if len(page.Messages) != 0 {
				page.More = true
				break
			}
			// Preserve a usable ID for an unusually escape-heavy body. Exact
			// reads return it intact rather than silently truncating content.
			view.Body, view.BodyOmitted = "", true
			encoded, _ = json.Marshal(view)
		}
		used += len(encoded) + 1
		page.Messages = append(page.Messages, view)
	}
	if page.More && len(page.Messages) > 0 {
		page.NextCursor = page.Messages[len(page.Messages)-1].ID
	}
	return page
}

func addTool[T any](server *mcp.Server, service *bus.Service, tool *mcp.Tool, call func(context.Context, bus.ToolArgs) (T, error)) {
	mcp.AddTool(server, tool, func(ctx context.Context, request *mcp.CallToolRequest, args bus.ToolArgs) (*mcp.CallToolResult, Output[T], error) {
		started := time.Now()
		value, err := call(ctx, args)
		service.ObserveTool(tool.Name, args.Actor, args.MessageID, time.Since(started), err)
		if err == nil && service.Runtime != nil {
			service.Runtime.ObserveActor(args.Actor)
		}
		output := Output[T]{Data: value, Runtime: runtimeWarning(service)}
		if err != nil {
			output.Error = &ToolError{Code: bus.ErrorCodeOf(err), Message: err.Error()}
			return &mcp.CallToolResult{IsError: true}, output, nil
		}
		return nil, output, nil
	})
}

func New(service *bus.Service) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "zalki", Version: bus.BuildVersion()}, &mcp.ServerOptions{
		Instructions: "Keep receipt bookkeeping silent; no ACK-only messages or ACK narration unless requested. " +
			"Share substantive results, reviews, questions, and decisions with the user and relevant agents. " +
			"Continue useful discussion; avoid repeating unchanged status. A sender's no-reply note does not cancel the user's task. " +
			"Check timestamps and later updates before acting on old requests. Acceptance is not acknowledgement. " +
			"End your turn for native delivery; do not poll or handle body_deferred metadata. " +
			"runtime.needs_restart means reconnect this server.",
	})
	for _, definition := range bus.Tools() {
		encoded, err := json.Marshal(definition)
		if err != nil {
			panic(err)
		}
		var tool mcp.Tool
		if err := json.Unmarshal(encoded, &tool); err != nil {
			panic(err)
		}
		// Clients require concrete output schemas; interface{} produces a boolean schema.
		switch tool.Name {
		case "list_agents":
			addTool(server, service, &tool, func(_ context.Context, _ bus.ToolArgs) ([]bus.AgentInfo, error) {
				return service.Store.Discover()
			})
		case "send_message":
			addTool(server, service, &tool, func(ctx context.Context, args bus.ToolArgs) (MessageReceipt, error) {
				return messageReceipt(service.Send(ctx, bus.SendRequest{
					Actor: args.Actor, Capability: args.Capability,
					To: args.To, MessageID: args.MessageID, Body: args.Body,
				}))
			})
		case "reply_message":
			addTool(server, service, &tool, func(ctx context.Context, args bus.ToolArgs) (MessageReceipt, error) {
				return messageReceipt(service.Reply(ctx, bus.ReplyRequest{
					Actor: args.Actor, Capability: args.Capability,
					MessageID: args.MessageID, Body: args.Body,
				}))
			})
		case "ack_message":
			addTool(server, service, &tool, func(_ context.Context, args bus.ToolArgs) (MessageReceipt, error) {
				return messageReceipt(service.Store.Ack(args.Actor, args.Capability, args.MessageID))
			})
		case "list_messages":
			addTool(server, service, &tool, func(_ context.Context, args bus.ToolArgs) (MessagePage, error) {
				messages, err := service.Store.ReadMessages(bus.MessageQuery{
					Actor: args.Actor, Capability: args.Capability,
					History: args.History, Limit: args.Limit,
					MessageID: args.MessageID, IncludeBody: args.IncludeBody, Cursor: args.Cursor,
				})
				return messagePage(messages, args.MessageID != ""), err
			})
		default:
			panic("missing handler for " + tool.Name)
		}
	}
	return server
}

func Run(ctx context.Context, service *bus.Service, input io.ReadCloser, output io.WriteCloser) error {
	return New(service).Run(ctx, &mcp.IOTransport{Reader: input, Writer: output, MaxLineLength: 128 * 1024})
}
