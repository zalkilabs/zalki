package bus

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/logging"
)

// Deliberately has no body, capability, name, path, output or free-text error
// fields. Log records are allowlisted metadata, not serialized domain objects.
type eventData struct {
	Actor, Recipient, MessageID, ReplyTo, Generation string
	Agent, HookEvent                                 string
	HookStage                                        string
	Status, ReceiptStatus                            DeliveryStatus
	State                                            SessionState
	Code                                             ErrorCode
	Err                                              error
	Duration                                         time.Duration
	Reused                                           bool
}

func (s Store) logEvent(level slog.Level, event string, data eventData) {
	if s.Logger == nil || !s.Logger.Enabled(context.Background(), level) {
		return
	}
	attrs := make([]slog.Attr, 0, 12)
	for _, field := range []struct{ key, value string }{
		{"message_id", data.MessageID}, {"reply_to", data.ReplyTo}, {"generation", data.Generation},
	} {
		if id, err := UUID(field.value); err == nil {
			attrs = append(attrs, slog.String(field.key, id))
		}
	}
	for _, field := range []struct{ key, value string }{{"actor", data.Actor}, {"recipient", data.Recipient}} {
		kind, id, ok := strings.Cut(field.value, ":")
		if _, err := s.registry().Lookup(kind); ok && err == nil {
			if canonical, err := UUID(id); err == nil {
				attrs = append(attrs, slog.String(field.key, kind+":"+canonical))
			}
		}
	}
	if _, err := s.registry().Lookup(data.Agent); err == nil {
		attrs = append(attrs, slog.String("agent", data.Agent))
	}
	if StandardEvents(true)[data.HookEvent] != "" {
		attrs = append(attrs, slog.String("hook_event", data.HookEvent))
	}
	switch data.HookStage {
	case "adapter", "decode", "capture", "event", "normalize", "identity", "owner", "handler", "register", "name", "startup", "output", "state":
		attrs = append(attrs, slog.String("hook_stage", data.HookStage))
	}
	if data.Status.valid() {
		attrs = append(attrs, slog.String("status", string(data.Status)))
	}
	if data.ReceiptStatus.valid() {
		attrs = append(attrs, slog.String("receipt_status", string(data.ReceiptStatus)))
	}
	if data.State.valid() {
		attrs = append(attrs, slog.String("state", string(data.State)))
	}
	code := data.Code
	if data.Err != nil {
		if code == "" {
			code = ErrorCodeOf(data.Err)
		}
		attrs = append(attrs, slog.String("error_kind", logging.ErrorKind(data.Err)))
	}
	if code != "" {
		switch code {
		case CodeInvalidInput, CodeInvalidIdentity, CodeUnauthorized, CodeConflict, CodeUnavailable, CodeNotFound, CodeUnsupportedSchema, CodeInvalidTransition, CodeInternal:
		default:
			code = CodeInternal
		}
		attrs = append(attrs, slog.String("error_code", string(code)))
	}
	if data.Duration > 0 {
		attrs = append(attrs, slog.Int64("duration_ms", data.Duration.Milliseconds()))
	}
	if data.Reused {
		attrs = append(attrs, slog.Bool("reused", true))
	}
	s.Logger.LogAttrs(context.Background(), level, event, attrs...)
}

func messageEvent(msg Message) eventData {
	return eventData{Actor: msg.From, Recipient: msg.To, MessageID: msg.ID, ReplyTo: msg.ReplyTo, Generation: msg.TargetGeneration, Status: msg.Status}
}

func (s Store) logReservation(msg Message) {
	level := slog.LevelInfo
	data := messageEvent(msg)
	if msg.Status == StatusApprovalBlocked || msg.Status == StatusNotSent {
		level = slog.LevelWarn
		data.Code = CodeUnavailable
	}
	s.logEvent(level, "message.reserved", data)
}

// ObserveTool is called at the MCP boundary. SDK input-schema failures happen
// before that boundary; request payloads and returned content are never logged.
func (s *Service) ObserveTool(name, actor, id string, elapsed time.Duration, err error) {
	known := false
	for _, tool := range Tools() {
		if tool["name"] == name {
			known = true
			break
		}
	}
	if !known {
		return
	}
	level := slog.LevelDebug
	if err != nil {
		level = slog.LevelWarn
	}
	s.Store.logEvent(level, "tool."+name, eventData{Actor: actor, MessageID: id, Duration: elapsed, Err: err})
}
