package bus

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s Store) message(id string) (Message, error) {
	var msg Message
	id, err := UUID(id)
	if err == nil {
		err = readRecord(filepath.Join(s.Dir, "messages", id+".json"), &msg, &msg.Schema)
	}
	if err == nil && msg.ID != id {
		err = fail(CodeInvalidIdentity, "message file identity mismatch")
	}
	return msg, err
}

// eachMessage visits journal files in the same lexical order as filepath.Glob.
// The caller holds the journal lock and chooses whether a read error is fatal,
// ignored, or reported. Exact-ID reads do not use this scan.
func (s Store) eachMessage(visit func(id string, message Message, readErr error) error) error {
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

func (s Store) saveMessage(v Message) error {
	if v.Schema < 0 || v.Schema > recordSchema {
		return fail(CodeUnsupportedSchema, "unsupported message schema %d", v.Schema)
	}
	if _, err := UUID(v.ID); err != nil {
		return err
	}
	v.Schema = recordSchema
	return writeJSON(filepath.Join(s.Dir, "messages", v.ID+".json"), v)
}

func (s Store) ReserveMessage(request SendRequest) (message Message, target Session, fresh bool, err error) {
	err = s.locked(func() error {
		var e error
		message, target, fresh, e = s.prepareMessage(request, true)
		return e
	})
	return
}

// prepareMessage is called with the journal lock held. Reply transactions stage
// the reservation without writing it until the durable intent is committed.
func (s Store) prepareMessage(request SendRequest, persist bool) (Message, Session, bool, error) {
	actor, key, to, id, body, replyTo := request.Actor, request.Capability, request.To, request.MessageID, request.Body, request.ReplyTo
	var message Message
	var target Session
	fresh := false
	canonical, err := UUID(id)
	if err != nil {
		return message, target, false, err
	}
	id = canonical
	if len(body) == 0 || len(body) > 16*1024 || strings.ContainsRune(body, 0) {
		return message, target, false, fail(CodeInvalidInput, "body must be 1..16384 UTF-8 bytes without NUL")
	}
	err = func() error {
		sender, err := s.authenticate(actor, key)
		if err != nil {
			return err
		}
		old, err := s.message(id)
		if err == nil {
			if old.From != actor || (old.To != to && (old.RequestedTo == "" || old.RequestedTo != to)) || old.Body != body || old.ReplyTo != replyTo {
				return fail(CodeConflict, "message UUID already used with different contents")
			}
			message = old
			s.logEvent(slog.LevelDebug, "message.reused", messageEvent(old))
			return nil // Never submit the same message twice, even after a crash.
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		requestedTo := to
		target, err = s.resolve(to)
		if err != nil {
			return err
		}
		if target.Retired {
			return fail(CodeUnavailable, "destination has retired")
		}
		if s.sessionLiveness(target) == LivenessExited {
			return fail(CodeUnavailable, "destination owner process has exited")
		}
		to = target.ID
		if actor == to {
			return fail(CodeInvalidInput, "self messages are not supported")
		}
		if replyTo != "" {
			parent, err := s.message(replyTo)
			if err != nil {
				return err
			}
			if parent.To != actor || parent.From != to {
				return fail(CodeUnauthorized, "reply must return to the original sender")
			}
		}
		message, err = s.outgoingMessage(sender, target, id, body, replyTo)
		if err != nil {
			return err
		}
		if requestedTo != to {
			message.RequestedTo = requestedTo
		}
		if target.State == StateApproval {
			if err := s.approvalCapacity(message.To); err != nil {
				return err
			}
		}
		if err := s.consumeFloodQuota(message, time.Now()); err != nil {
			if moveErr := message.settleNotSent(ErrorCodeOf(err), err.Error()); moveErr != nil {
				return moveErr
			}
			return err
		}
		if target.State == StateApproval {
			if err := message.hold(); err != nil {
				return err
			}
		}
		fresh = target.State != StateApproval
		if persist {
			if err := s.saveReservation(message); err != nil {
				return err
			}
			s.logReservation(message)
		}
		return nil
	}()
	return message, target, fresh, err
}

func (s Store) outgoingMessage(sender, target Session, id, body, replyTo string) (Message, error) {
	senders := []Session{sender}
	s.nativeNames(senders)
	adapter, err := s.registry().Lookup(sender.Kind)
	if err != nil {
		return Message{}, err
	}
	return Message{Schema: recordSchema, ID: id, From: sender.ID, FromName: senders[0].Name, FromLabel: adapter.Descriptor().Label, To: target.ID, Body: body, ReplyTo: replyTo, TargetGeneration: target.Generation, CreatedAt: timestamp(), Status: StatusAttempting}, nil
}

func (s Store) Finish(id string, receipt Receipt) (Message, error) {
	var msg Message
	err := s.locked(func() error {
		var err error
		msg, err = s.message(id)
		if err != nil {
			return err
		}
		if err := msg.applyReceipt(receipt); err != nil {
			return err
		}
		receipt = *msg.Receipt
		if err := s.saveMessage(msg); err != nil {
			return err
		} // Preserve a racing ACK.
		data := messageEvent(msg)
		data.ReceiptStatus, data.Code = receipt.Status, receipt.Code
		data.Err = receipt.cause
		level := slog.LevelDebug
		if msg.Status == StatusNotSent {
			level = slog.LevelWarn
		}
		s.logEvent(level, "message.receipt_saved", data)
		return nil
	})
	return msg, err
}

func (s Store) Ack(actor, key, id string) (result Message, resultErr error) {
	defer func() {
		if resultErr != nil {
			s.logEvent(slog.LevelWarn, "message.ack_failed", eventData{Actor: actor, MessageID: id, Err: resultErr})
		}
	}()
	var msg Message
	err := s.locked(func() error {
		if _, err := s.authenticate(actor, key); err != nil {
			return err
		}
		var err error
		msg, err = s.message(id)
		if err != nil {
			return err
		}
		if msg.To != actor {
			return fail(CodeUnauthorized, "only the bound recipient can acknowledge")
		}
		if !msg.Status.canAcknowledge() {
			return fail(CodeInvalidTransition, "cannot acknowledge a message that was not sent")
		}
		first := msg.AcknowledgedAt == ""
		if first {
			msg.AcknowledgedAt = timestamp()
		}
		if err := s.saveMessage(msg); err != nil {
			return err
		}
		if first {
			s.logEvent(slog.LevelInfo, "message.acknowledged", messageEvent(msg))
		}
		return nil
	})
	return msg, err
}
