package bus

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/logging"
	"github.com/zalkilabs/zalki/internal/model"
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

// logEvent writes one log record made only of allowlisted attributes. The
// attributes are always written in this order: ids, actor and recipient, agent
// and hook, statuses, error, then duration and reuse. A nil logger, or a level
// it does not log, does no work.
func (s Store) logEvent(level slog.Level, event string, data eventData) {
	if s.Logger == nil || !s.Logger.Enabled(context.Background(), level) {
		return
	}
	attrs := make([]slog.Attr, 0, 12)
	attrs = appendIDAttrs(attrs, data)
	attrs = s.appendActorAttrs(attrs, data)
	attrs = s.appendAgentAndHookAttrs(attrs, data)
	attrs = appendStatusAttrs(attrs, data)
	attrs = appendErrorAttrs(attrs, data)
	attrs = appendDurationAndReuseAttrs(attrs, data)
	s.Logger.LogAttrs(context.Background(), level, event, attrs...)
}

// appendIDAttrs adds message_id, reply_to and generation, each only if it is a
// UUID, written in lower case.
func appendIDAttrs(attrs []slog.Attr, data eventData) []slog.Attr {
	for _, field := range []struct{ key, value string }{
		{"message_id", data.MessageID}, {"reply_to", data.ReplyTo}, {"generation", data.Generation},
	} {
		if id, err := UUID(field.value); err == nil {
			attrs = append(attrs, slog.String(field.key, id))
		}
	}
	return attrs
}

// appendActorAttrs adds actor and recipient, each only as "<kind>:<uuid>" with a
// registered adapter kind, written with a lower-case UUID.
func (s Store) appendActorAttrs(attrs []slog.Attr, data eventData) []slog.Attr {
	for _, field := range []struct{ key, value string }{{"actor", data.Actor}, {"recipient", data.Recipient}} {
		kind, id, ok := strings.Cut(field.value, ":")
		if _, err := s.registry().Lookup(kind); ok && err == nil {
			if canonical, err := UUID(id); err == nil {
				attrs = append(attrs, slog.String(field.key, kind+":"+canonical))
			}
		}
	}
	return attrs
}

// appendAgentAndHookAttrs adds agent (a registered kind), hook_event (a
// standard event) and hook_stage (one of a fixed list of words).
func (s Store) appendAgentAndHookAttrs(attrs []slog.Attr, data eventData) []slog.Attr {
	if _, err := s.registry().Lookup(data.Agent); err == nil {
		attrs = append(attrs, slog.String("agent", data.Agent))
	}
	if agent.StandardEvents(true)[data.HookEvent] != "" {
		attrs = append(attrs, slog.String("hook_event", data.HookEvent))
	}
	switch data.HookStage {
	case "adapter", "decode", "capture", "event", "normalize", "identity", "owner", "handler", "watch", "approval", "register", "name", "startup", "output", "state":
		attrs = append(attrs, slog.String("hook_stage", data.HookStage))
	}
	return attrs
}

// appendStatusAttrs adds status, receipt_status and state, each only if valid.
func appendStatusAttrs(attrs []slog.Attr, data eventData) []slog.Attr {
	if data.Status.Valid() {
		attrs = append(attrs, slog.String("status", string(data.Status)))
	}
	if data.ReceiptStatus.Valid() {
		attrs = append(attrs, slog.String("receipt_status", string(data.ReceiptStatus)))
	}
	if data.State.Valid() {
		attrs = append(attrs, slog.String("state", string(data.State)))
	}
	return attrs
}

// appendErrorAttrs adds error_kind (the class of Err, never its text) and
// error_code. The code is Code, or the code of Err when Code is empty. A code
// outside the known ones is written as "internal"; it does not fall back to Err.
func appendErrorAttrs(attrs []slog.Attr, data eventData) []slog.Attr {
	code := data.Code
	if data.Err != nil {
		if code == "" {
			code = ErrorCodeOf(data.Err)
		}
		attrs = append(attrs, slog.String("error_kind", logging.ErrorKind(data.Err)))
	}
	if code != "" {
		switch code {
		case model.CodeInvalidInput, model.CodeInvalidIdentity, model.CodeUnauthorized, model.CodeConflict, model.CodeUnavailable, model.CodeRateLimited, model.CodeNotFound, model.CodeUnsupportedSchema, model.CodeInvalidTransition, model.CodeInternal:
		default:
			code = model.CodeInternal
		}
		attrs = append(attrs, slog.String("error_code", string(code)))
	}
	return attrs
}

// appendDurationAndReuseAttrs adds duration_ms (only above zero) and reused
// (only when true).
func appendDurationAndReuseAttrs(attrs []slog.Attr, data eventData) []slog.Attr {
	if data.Duration > 0 {
		attrs = append(attrs, slog.Int64("duration_ms", data.Duration.Milliseconds()))
	}
	if data.Reused {
		attrs = append(attrs, slog.Bool("reused", true))
	}
	return attrs
}

func messageEvent(msg Message) eventData {
	return eventData{Actor: msg.From, Recipient: msg.To, MessageID: msg.ID, ReplyTo: msg.ReplyTo, Generation: msg.TargetGeneration, Status: msg.Status}
}

func (s Store) logReservation(msg Message) {
	level := slog.LevelInfo
	data := messageEvent(msg)
	if msg.Status == model.StatusApprovalBlocked || msg.Status == model.StatusNotSent {
		level = slog.LevelWarn
		data.Code = model.CodeUnavailable
	}
	s.logEvent(level, "message.reserved", data)
}

// ObserveTool is called at the MCP boundary. SDK input-schema failures happen
// before that boundary; request payloads and returned content are never logged.
// Only registered handlers call this method, with their declared tool name.
func (s *Service) ObserveTool(name, actor, id string, elapsed time.Duration, err error) {
	level := slog.LevelDebug
	if err != nil {
		level = slog.LevelWarn
	}
	s.Store.logEvent(level, "tool."+name, eventData{Actor: actor, MessageID: id, Duration: elapsed, Err: err})
}
