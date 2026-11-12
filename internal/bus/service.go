package bus

import (
	"context"
	"log/slog"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
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
	// PluginInbox is the composition root's local inbox admission policy.
	PluginInbox pluginInboxAdmit
	// PluginSend is the composition root's plugin send-grant check.
	PluginSend pluginSendAdmit
	// Deliver overrides native-inbox transports in isolated tests.
	Deliver func(context.Context, model.Message, model.Session) model.Receipt
}

func (s *Service) Send(ctx context.Context, request SendRequest) (result model.Message, err error) {
	if request.MessageID == "" {
		request.MessageID = newID()
	}
	actor, key, to, id, body := request.Actor, request.Capability, request.To, request.MessageID, request.Body
	defer func() {
		if err != nil {
			s.Store.logEvent(slog.LevelWarn, "message.send_failed", eventData{Actor: actor, Recipient: to, MessageID: id, Err: err})
		}
	}()
	if err := model.ValidateMessageBody(body, key); err != nil {
		return model.Message{}, err
	}
	if model.IsPluginInboxAddress(to) {
		return submitPluginInbox(s.Store, request, s.PluginInbox)
	}
	msg, target, fresh, err := s.Store.ReserveMessage(request)
	if err != nil || !fresh {
		return msg, err
	}
	return s.deliverReserved(ctx, msg, target)
}

func (s *Service) deliverReserved(ctx context.Context, msg model.Message, target model.Session) (model.Message, error) {
	id := msg.ID
	adapter, err := s.Store.registry().Lookup(target.Kind)
	if err != nil {
		return model.Message{}, err
	}
	current, held, err := s.revalidateTarget(id, target)
	if held.ID != "" {
		return held, nil
	}
	if err != nil {
		return s.Store.Finish(id, model.Receipt{At: model.Timestamp(), Status: model.StatusNotSent, Error: err.Error()}.WithCause(err))
	}
	target = current
	if target.Receive.HookDelivery {
		msg, err = s.Store.Finish(id, model.Receipt{At: model.Timestamp(), Status: model.StatusPendingActivity})
		if err != nil {
			return msg, err
		}
	}
	deliver := adapter.Deliver
	target.KeyHash = ""
	if s.Deliver != nil && target.Receive.Inbox == model.InboxNative {
		deliver = s.Deliver
	}
	receipt := s.runTransport(ctx, deliver, msg, target)
	result, err := s.Store.Finish(id, receipt)
	if err == nil && target.State == model.StateBusy {
		result.BusyDelivery = target.Receive.BusyDelivery
	}
	return result, err
}

// revalidateTarget rechecks the destination under the journal lock, since the
// registration may have changed after reservation. A target in approval state
// is not delivered to: the message is held and returned instead.
func (s *Service) revalidateTarget(id string, target model.Session) (current model.Session, held model.Message, err error) {
	err = s.Store.locked(func() error {
		var err error
		current, err = s.Store.session(target.ID)
		if err != nil {
			return err
		}
		if current.Retired || current.Generation != target.Generation {
			return model.NewError(model.CodeUnavailable, "destination registration changed before delivery")
		}
		if s.Store.sessionLiveness(current) == model.LivenessExited {
			return model.NewError(model.CodeUnavailable, "destination owner process exited before delivery")
		}
		if current.State != model.StateApproval {
			return nil
		}
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
	})
	return current, held, err
}

// runTransport calls the adapter and records the outcome without message content.
func (s *Service) runTransport(ctx context.Context, deliver func(context.Context, model.Message, model.Session) model.Receipt, msg model.Message, target model.Session) model.Receipt {
	s.Store.logEvent(slog.LevelDebug, "transport.started", messageEvent(msg))
	started := time.Now()
	receipt := deliver(ctx, s.Store.withLabel(msg), target)
	data := messageEvent(msg)
	data.ReceiptStatus = receipt.Status
	data.Code = receipt.Code
	data.Err = receipt.Cause()
	data.Agent = target.Kind
	if receipt.Error != "" && data.Code == "" {
		data.Code = model.CodeUnavailable
	}
	data.Duration = time.Since(started)
	s.Store.logEvent(receiptLogLevel(receipt), "transport.completed", data)
	return receipt
}

// receiptLogLevel warns for outcomes where delivery failed or is uncertain.
func receiptLogLevel(receipt model.Receipt) slog.Level {
	switch receipt.Status {
	case model.StatusNotSent, model.StatusCommandFailed, model.StatusUnknownTimeout, model.StatusUnknownWrite, model.StatusWakeUncertain:
		return slog.LevelWarn
	case model.StatusAttempting, model.StatusSubmitted, model.StatusTransportWritten, model.StatusPendingActivity,
		model.StatusWakeWritten, model.StatusContextEmitted, model.StatusApprovalBlocked, model.StatusQueuedForApproval:
		// These outcomes still warn when the receipt carries public error text.
	}
	if receipt.Error != "" {
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// Reply uses the journal's exact sender, never the displayed label. Repeating
// the same incoming ID and body reuses the outgoing ID without resubmission.
func (s *Service) Reply(ctx context.Context, request ReplyRequest) (result model.Message, err error) {
	actor, key, id, body := request.Actor, request.Capability, request.MessageID, request.Body
	defer func() {
		if err != nil {
			s.Store.logEvent(slog.LevelWarn, "message.reply_failed", eventData{Actor: actor, MessageID: id, Err: err})
		}
	}()
	if err := model.ValidateMessageBody(body, key); err != nil {
		return model.Message{}, err
	}
	msg, target, fresh, err := s.Store.prepareReply(request)
	if err != nil || !fresh {
		return msg, err
	}
	return s.deliverReserved(ctx, msg, target)
}
