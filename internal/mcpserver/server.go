// Package mcpserver handles MCP over stdio.
package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
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
	Note         string             `json:"note,omitempty"`
	Error        string             `json:"error,omitempty"`
	Code         bus.ErrorCode      `json:"code,omitempty"`
}

func messageReceipt(message bus.Message, err error) (MessageReceipt, error) {
	receipt := MessageReceipt{ID: message.ID, Status: message.Status, Acknowledged: message.AcknowledgedAt != ""}
	if message.Receipt != nil {
		receipt.Error = message.Receipt.Error
		receipt.Code = message.Receipt.Code
	}
	if err != nil || receipt.Acknowledged {
		return receipt, err
	}
	if message.IsQueuedForApproval() {
		receipt.Status = model.StatusQueuedForApproval
		receipt.Note = "Queued until the target's permission prompt is answered."
		receipt.Error, receipt.Code = "", ""
		return receipt, err
	}
	switch message.Status {
	case model.StatusApprovalBlocked:
		receipt.Note = "Not sent: target awaiting approval; no automatic retry."
	case model.StatusPendingActivity:
		receipt.Note = "Deferred until the next delivery hook."
	case model.StatusSubmitted, model.StatusTransportWritten, model.StatusWakeWritten:
		switch message.BusyDelivery {
		case model.DeliveryBetweenTools:
			receipt.Note = "Target busy; delivery between tool calls."
		case model.DeliveryNextTurn:
			receipt.Note = "Target busy; delivery at the next turn."
		case model.DeliveryNextHook:
			receipt.Note = "Target busy; delivery at the next hook."
		case model.DeliveryNotification:
			receipt.Note = "Target busy; native notification delivery."
		}
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
	Kind           string             `json:"kind,omitempty"`
	To             string             `json:"to"`
	Body           string             `json:"body,omitempty"`
	Preview        string             `json:"preview,omitempty"`
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
		view := MessageView{ID: msg.ID, From: msg.From, FromName: msg.FromName, FromLabel: msg.FromLabel, Kind: msg.Kind,
			To: msg.To, Body: msg.Body, Preview: msg.BodyPreview, BodyDeferred: msg.BodyDeferred, BodyOmitted: msg.BodyOmitted, ReplyTo: msg.ReplyTo,
			CreatedAt: msg.CreatedAt, Status: msg.Status, AcknowledgedAt: msg.AcknowledgedAt}
		if msg.Receipt != nil {
			view.Error, view.Code = msg.Receipt.Error, msg.Receipt.Code
		}
		if msg.IsQueuedForApproval() {
			view.Status = model.StatusQueuedForApproval
			view.Error, view.Code = "", ""
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

// DiscoveryOutput adds one warning count to discovery, keeping healthy data
// arrays and all other tools' envelopes byte-identical.
type DiscoveryOutput struct {
	Data            []bus.AgentInfo    `json:"data"`
	SkippedSessions int                `json:"skipped_sessions,omitempty"`
	Runtime         *bus.RuntimeStatus `json:"runtime,omitempty"`
	Error           *ToolError         `json:"error,omitempty"`
}

type discoveryResult struct {
	agents  []bus.AgentInfo
	skipped int
}

func toolOutput[T any](service *bus.Service, value T, err error) Output[T] {
	output := Output[T]{Data: value, Runtime: runtimeWarning(service)}
	if err != nil {
		output.Error = &ToolError{Code: bus.ErrorCodeOf(err), Message: err.Error()}
	}
	return output
}

func discoveryOutput(service *bus.Service, value discoveryResult, err error) DiscoveryOutput {
	output := toolOutput(service, value.agents, err)
	return DiscoveryOutput{Data: output.Data, SkippedSessions: value.skipped, Runtime: output.Runtime, Error: output.Error}
}

func addToolOutput[T, O any](server *mcp.Server, service *bus.Service, tool *mcp.Tool, call func(context.Context, toolArgs) (T, error), wrap func(*bus.Service, T, error) O) {
	mcp.AddTool(server, tool, func(ctx context.Context, request *mcp.CallToolRequest, args toolArgs) (*mcp.CallToolResult, O, error) {
		started := time.Now()
		value, err := call(ctx, args)
		service.ObserveTool(tool.Name, args.Actor, args.MessageID, time.Since(started), err)
		if err == nil && service.Runtime != nil {
			service.Runtime.ObserveActor(args.Actor)
		}
		output := wrap(service, value, err)
		if err != nil {
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
			"Continue useful work; yield when waiting for deferred or next-turn delivery. Never poll or act on body_deferred metadata. " +
			"runtime.needs_restart means reconnect this server.",
	})
	definitions := toolTable()
	server.AddReceivingMiddleware(argumentHints(definitions))
	for _, definition := range definitions {
		definition.register(server, service)
	}
	return server
}

func Run(ctx context.Context, service *bus.Service, input io.ReadCloser, output io.WriteCloser) error {
	return New(service).Run(ctx, &mcp.IOTransport{Reader: input, Writer: output, MaxLineLength: 128 * 1024})
}
