package bus

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func (s Store) message(id string) (model.Message, error) {
	var msg model.Message
	id, err := model.UUID(id)
	if err == nil {
		err = readRecord(filepath.Join(s.Dir, "messages", id+".json"), &msg, &msg.Schema)
	}
	if err == nil && msg.ID != id {
		err = model.NewError(model.CodeInvalidIdentity, "message file identity mismatch")
	}
	return msg, err
}

// eachMessage visits journal files in the same lexical order as filepath.Glob.
// The caller holds the journal lock and chooses whether a read error is fatal,
// ignored, or reported. Exact-ID reads do not use this scan.
func (s Store) eachMessage(visit func(id string, message model.Message, readErr error) error) error {
	files, err := filepath.Glob(filepath.Join(s.Dir, "messages", "*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		id := strings.TrimSuffix(filepath.Base(file), ".json")
		message, readErr := s.message(id)
		if err := visit(id, message, readErr); err != nil {
			return err
		}
	}
	return nil
}

// eachReadableMessage visits every readable message file and returns the files
// it skipped as unreadable or unsupported, so one such file cannot stop a scan.
// The caller holds the journal lock and reports the skipped files as it needs.
func (s Store) eachReadableMessage(visit func(model.Message) error) (skippedMessages, error) {
	var skipped skippedMessages
	err := s.eachMessage(func(id string, msg model.Message, readErr error) error {
		if readErr != nil {
			skipped.add(id)
		} else if err := visit(msg); err != nil {
			return err
		}
		return nil
	})
	return skipped, err
}

func (s Store) saveMessage(v model.Message) error {
	return s.saveMessageRecord(v)
}

func (s Store) saveMessageRecord(v model.Message) error {
	if _, err := model.UUID(v.ID); err != nil {
		return err
	}
	v.Schema = recordSchema
	// Keep markers ⊇ open records; see message_open.go.
	open := openMessage(v)
	if open {
		if err := markers(s).add(v.ID, true); err != nil {
			return err
		}
	}
	if err := s.writeMessage(filepath.Join(s.Dir, "messages", v.ID+".json"), v); err != nil {
		return err
	}
	if !open {
		markers(s).drop(v.ID)
	}
	return nil
}

func (s Store) ReserveMessage(request SendRequest) (message model.Message, target model.Session, fresh bool, err error) {
	err = s.locked(func() error {
		var e error
		message, target, fresh, e = s.prepareMessage(request, true)
		return e
	})
	return
}

// prepareMessage is called with the journal lock held. Reply transactions stage
// the reservation without writing it until the durable intent is committed.
func (s Store) prepareMessage(request SendRequest, persist bool) (message model.Message, target model.Session, fresh bool, err error) {
	id, err := model.UUID(request.MessageID)
	if err != nil {
		return message, target, false, err
	}
	request.MessageID = id
	if len(request.Body) == 0 || len(request.Body) > model.MaxMessageBodyBytes || strings.ContainsRune(request.Body, 0) {
		return message, target, false, model.NewError(model.CodeInvalidInput, "body must be 1..16384 UTF-8 bytes without NUL")
	}
	sender, err := s.authenticate(request.Actor, request.Capability)
	if err != nil {
		return message, target, false, err
	}
	old, err := s.message(id)
	if err == nil {
		if old.From != request.Actor || (old.To != request.To && (old.RequestedTo == "" || old.RequestedTo != request.To)) || old.Body != request.Body || old.ReplyTo != request.ReplyTo {
			return message, target, false, model.NewError(model.CodeConflict, "message UUID already used with different contents")
		}
		s.logEvent(slog.LevelDebug, "message.reused", messageEvent(old))
		return old, target, false, nil // Never submit the same message twice, even after a crash.
	}
	if !errors.Is(err, os.ErrNotExist) {
		return message, target, false, err
	}
	if err := checkAdmissionBody(request.Body, s.MaxBodyBytes); err != nil {
		return message, target, false, err
	}
	return s.reserveNewMessage(sender, request, persist)
}

// reserveNewMessage validates the destination and sender limits for an unused
// message ID, then stages the message, held if the destination awaits approval.
// fresh reports that the message is ready for delivery now.
func (s Store) reserveNewMessage(sender model.Session, request SendRequest, persist bool) (message model.Message, target model.Session, fresh bool, err error) {
	actor, id, body, replyTo := request.Actor, request.MessageID, request.Body, request.ReplyTo
	requestedTo := request.To
	target, err = s.resolve(requestedTo)
	if err != nil {
		return message, target, false, err
	}
	if err := validateReservationTarget(s, target, actor, replyTo); err != nil {
		return message, target, false, err
	}
	message, err = s.outgoingMessage(sender, target, id, body, replyTo)
	if err != nil {
		return message, target, false, err
	}
	if requestedTo != target.ID {
		message.RequestedTo = requestedTo
	}
	if err := prepareReservation(s, &message, target); err != nil {
		return message, target, false, err
	}
	fresh = target.State != model.StateApproval
	if persist {
		if err := s.saveReservation(message); err != nil {
			return message, target, fresh, err
		}
		s.logReservation(message)
	}
	return message, target, fresh, nil
}

func validateReservationTarget(s Store, target model.Session, actor, replyTo string) error {
	if target.Retired {
		return model.NewError(model.CodeUnavailable, "destination has retired")
	}
	if s.sessionLiveness(target) == model.LivenessExited {
		return model.NewError(model.CodeUnavailable, "destination owner process has exited")
	}
	to := target.ID
	if actor == to {
		return model.NewError(model.CodeInvalidInput, "self messages are not supported")
	}
	if replyTo != "" {
		parent, err := s.message(replyTo)
		if err != nil {
			return err
		}
		if parent.To != actor || parent.From != to {
			return model.NewError(model.CodeUnauthorized, "reply must return to the original sender")
		}
	}
	return nil
}

func prepareReservation(s Store, message *model.Message, target model.Session) error {
	if target.State == model.StateApproval {
		if err := s.approvalCapacity(message.To); err != nil {
			return err
		}
	}
	if err := s.consumeFloodQuota(*message, time.Now()); err != nil {
		if moveErr := message.SettleNotSent(model.ErrorCodeOf(err), err.Error()); moveErr != nil {
			return moveErr
		}
		return err
	}
	if target.State == model.StateApproval {
		if err := message.Hold(); err != nil {
			return err
		}
	}
	return nil
}

func (s Store) outgoingMessage(sender, target model.Session, id, body, replyTo string) (model.Message, error) {
	senders := []model.Session{sender}
	s.nativeNames(senders)
	adapter, err := s.registry().Lookup(sender.Kind)
	if err != nil {
		return model.Message{}, err
	}
	return model.Message{Schema: recordSchema, ID: id, From: sender.ID, FromName: senders[0].Name, FromLabel: adapter.Descriptor().Label, To: target.ID, Body: body, ReplyTo: replyTo, TargetGeneration: target.Generation, CreatedAt: model.Timestamp(), Status: model.StatusAttempting}, nil
}

func (s Store) Finish(id string, receipt model.Receipt) (model.Message, error) {
	var msg model.Message
	err := s.locked(func() error {
		var err error
		msg, err = s.message(id)
		if err != nil {
			return err
		}
		candidate := msg
		if err := candidate.ApplyReceipt(receipt); err != nil {
			return err
		}
		if err := s.saveMessage(candidate); err != nil {
			data := messageEvent(msg)
			data.Err, data.ReceiptStatus = err, receipt.Status
			s.logEvent(slog.LevelWarn, "message.receipt_save_failed", data)
			return err
		}
		msg = candidate
		receipt = *msg.Receipt // Preserve a racing ACK and report only durable state.
		data := messageEvent(msg)
		data.ReceiptStatus, data.Code = receipt.Status, receipt.Code
		data.Err = receipt.Cause()
		level := slog.LevelDebug
		if msg.Status == model.StatusNotSent {
			level = slog.LevelWarn
		}
		s.logEvent(level, "message.receipt_saved", data)
		return nil
	})
	return msg, err
}

func (s Store) Ack(actor, key, id string) (result model.Message, resultErr error) {
	defer func() {
		if resultErr != nil {
			s.logEvent(slog.LevelWarn, "message.ack_failed", eventData{Actor: actor, MessageID: id, Err: resultErr})
		}
	}()
	var msg model.Message
	err := s.locked(func() error {
		if _, err := s.authenticate(actor, key); err != nil {
			return err
		}
		if _, err := model.UUID(id); err != nil {
			return err
		}
		loaded, err := s.message(id)
		if err != nil || model.IsPluginInboxAddress(loaded.To) || loaded.To != actor {
			return model.NewError(model.CodeNotFound, "message not found or not yours")
		}
		msg = loaded
		if !msg.Status.CanAcknowledge() {
			return model.NewError(model.CodeInvalidTransition, "cannot acknowledge a message that was not sent")
		}
		first := msg.AcknowledgedAt == ""
		candidate := msg
		if first {
			candidate.AcknowledgedAt = model.Timestamp()
		}
		if err := s.saveMessage(candidate); err != nil {
			return err
		}
		msg = candidate
		if first {
			s.logEvent(slog.LevelInfo, "message.acknowledged", messageEvent(msg))
		}
		return nil
	})
	return msg, err
}
