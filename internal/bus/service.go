package bus

import (
	"context"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"
)

// SendRequest identifies a new message and its authenticated sender.
type SendRequest struct {
	Actor, Capability            string
	To, MessageID, Body, ReplyTo string
}

type ReplyRequest struct {
	Actor, Capability string
	MessageID, Body   string
}

type Service struct {
	Store   Store
	Runtime *RuntimeLease
	// Deliver overrides native-inbox transports in isolated tests.
	Deliver func(context.Context, Message, Session) Receipt
}

func (s *Service) Send(ctx context.Context, request SendRequest) (result Message, err error) {
	if request.MessageID == "" {
		request.MessageID = newID()
	}
	actor, key, to, id, body := request.Actor, request.Capability, request.To, request.MessageID, request.Body
	defer func() {
		if err != nil {
			s.Store.logEvent(slog.LevelWarn, "message.send_failed", eventData{Actor: actor, Recipient: to, MessageID: id, Err: err})
		}
	}()
	if err := validateMessageBody(body, key); err != nil {
		return Message{}, err
	}
	msg, target, fresh, err := s.Store.ReserveMessage(request)
	if err != nil || !fresh {
		return msg, err
	}
	return s.deliverReserved(ctx, msg, target)
}

func (s *Service) deliverReserved(ctx context.Context, msg Message, target Session) (Message, error) {
	id := msg.ID
	adapter, err := s.Store.registry().Lookup(target.Kind)
	if err != nil {
		return Message{}, err
	}
	var held Message
	err = s.Store.locked(func() error {
		current, err := s.Store.session(target.ID)
		if err != nil {
			return err
		}
		if current.Retired || current.Generation != target.Generation {
			return fail(CodeUnavailable, "destination registration changed before delivery")
		}
		if s.Store.sessionLiveness(current) == LivenessExited {
			return fail(CodeUnavailable, "destination owner process exited before delivery")
		}
		if current.State == StateApproval {
			stored, err := s.Store.message(id)
			if err != nil {
				return err
			}
			if err := s.Store.holdForApproval(&stored); err != nil {
				return err
			}
			if err := s.Store.saveReservation(stored); err != nil {
				return err
			}
			held = stored
			return nil
		}
		target = current
		return nil
	})
	if held.ID != "" {
		return held, nil
	}
	if err != nil {
		return s.Store.Finish(id, Receipt{At: timestamp(), Status: StatusNotSent, Error: err.Error(), cause: err})
	}
	if target.Receive.HookDelivery {
		msg, err = s.Store.Finish(id, Receipt{At: timestamp(), Status: StatusPendingActivity})
		if err != nil {
			return msg, err
		}
	}
	deliver := adapter.Deliver
	target.KeyHash = ""
	if s.Deliver != nil && target.Receive.Inbox == InboxNative {
		deliver = s.Deliver
	}
	s.Store.logEvent(slog.LevelDebug, "transport.started", messageEvent(msg))
	started := time.Now()
	receipt := deliver(ctx, s.Store.withLabel(msg), target)
	data := messageEvent(msg)
	data.ReceiptStatus = receipt.Status
	data.Code = receipt.Code
	data.Err = receipt.cause
	data.Agent = target.Kind
	if receipt.Error != "" && data.Code == "" {
		data.Code = CodeUnavailable
	}
	data.Duration = time.Since(started)
	level := slog.LevelInfo
	if receipt.Error != "" || receipt.Status == StatusNotSent || receipt.Status == StatusCommandFailed || receipt.Status == StatusUnknownTimeout || receipt.Status == StatusUnknownWrite || receipt.Status == StatusWakeUncertain {
		level = slog.LevelWarn
	}
	s.Store.logEvent(level, "transport.completed", data)
	result, err := s.Store.Finish(id, receipt)
	if err == nil && target.State == StateBusy {
		result.BusyDelivery = target.Receive.BusyDelivery
	}
	return result, err
}

func validateMessageBody(body, key string) error {
	if !utf8.ValidString(body) {
		return fail(CodeInvalidInput, "body is not valid UTF-8")
	}
	if len(body) == 0 || len(body) > 16*1024 || strings.ContainsRune(body, 0) {
		return fail(CodeInvalidInput, "body must be 1..16384 UTF-8 bytes without NUL")
	}
	if key != "" && strings.Contains(body, key) {
		return fail(CodeInvalidInput, "refusing to include your session capability in an agent message")
	}
	return nil
}

// Reply uses the journal's exact sender, never the displayed label. Repeating
// the same incoming ID and body reuses the outgoing ID without resubmission.
func (s *Service) Reply(ctx context.Context, request ReplyRequest) (result Message, err error) {
	actor, key, id, body := request.Actor, request.Capability, request.MessageID, request.Body
	defer func() {
		if err != nil {
			s.Store.logEvent(slog.LevelWarn, "message.reply_failed", eventData{Actor: actor, MessageID: id, Err: err})
		}
	}()
	if err := validateMessageBody(body, key); err != nil {
		return Message{}, err
	}
	msg, target, fresh, err := s.Store.prepareReply(request)
	if err != nil || !fresh {
		return msg, err
	}
	return s.deliverReserved(ctx, msg, target)
}
