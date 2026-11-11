package bus

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"

	"github.com/zalkilabs/zalki/internal/model"
)

type replyTransaction struct {
	Schema         int           `json:"schema"`
	ParentID       string        `json:"parent_id"`
	AcknowledgedAt string        `json:"acknowledged_at"`
	Reply          model.Message `json:"reply"`
}

// Bind reply IDs to sender, parent, and body so retries reuse the reservation.
func replyID(actor, parent, body string) string {
	h := sha256.Sum256([]byte("zalki/reply/v1\x00" + actor + "\x00" + parent + "\x00" + body))
	h[6], h[8] = h[6]&15|128, h[8]&63|128
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// prepareReply reserves a reply and commits its durable intent under the journal
// lock. The results are filled in even when it fails, as the caller needs the
// reservation it was refused with (see prepareReplyLocked).
func (s Store) prepareReply(request ReplyRequest) (msg model.Message, target model.Session, fresh bool, err error) {
	err = s.locked(func() error {
		var lockedErr error
		msg, target, fresh, lockedErr = s.prepareReplyLocked(request)
		return lockedErr
	})
	return
}

// prepareReplyLocked checks, in this order: the actor (authenticate), the
// parent message (exists, is for the actor, is not an automatic notice, can
// still be acknowledged), then reserves the reply (reserveReply) and commits the
// intent (commitReplyIntent). Every refusal before the reservation leaves the
// journal as it was.
func (s Store) prepareReplyLocked(request ReplyRequest) (msg model.Message, target model.Session, fresh bool, err error) {
	sender, err := s.authenticate(request.Actor, request.Capability)
	if err != nil {
		return msg, target, fresh, err
	}
	parent, err := s.replyParent(request.Actor, request.MessageID)
	if err != nil {
		return msg, target, fresh, err
	}
	msg, target, fresh, err = s.reserveReply(sender, request, parent)
	if err != nil {
		return msg, target, fresh, err
	}
	return msg, target, fresh, s.commitReplyIntent(parent, msg, fresh)
}

// replyParent loads the message being answered. Only its recipient may reply,
// an automatic notice cannot be answered, and a message that was not sent
// cannot be acknowledged.
func (s Store) replyParent(actor, id string) (model.Message, error) {
	parent, err := s.message(id)
	if err != nil {
		return parent, err
	}
	if parent.To != actor {
		return parent, model.NewError(model.CodeUnauthorized, "only the bound recipient can reply")
	}
	if parent.Kind == model.NoticeKind {
		return parent, model.NewError(model.CodeInvalidInput, "this is an automatic notice; use send_message to %s", parent.From)
	}
	if !parent.Status.CanAcknowledge() {
		return parent, model.NewError(model.CodeInvalidTransition, "cannot acknowledge a message that was not sent")
	}
	return parent, nil
}

// reserveReply stages the reply message to the parent's sender. If the
// destination is unavailable or gone, the reply is still recorded, as not sent,
// and the parent is still acknowledged by the caller (characterized, not
// endorsed): see refuseReply.
func (s Store) reserveReply(sender model.Session, request ReplyRequest, parent model.Message) (msg model.Message, target model.Session, fresh bool, err error) {
	actor, body := request.Actor, request.Body
	msg, target, fresh, err = s.prepareMessage(SendRequest{Actor: actor, Capability: request.Capability, To: parent.From, MessageID: replyID(actor, parent.ID, body), Body: body, ReplyTo: parent.ID}, false)
	if code := model.ErrorCodeOf(err); code == model.CodeUnavailable || code == model.CodeNotFound {
		return s.refuseReply(sender, actor, parent, body, target, code, err.Error())
	}
	return msg, target, fresh, err
}

// refuseReply records a reply to an unavailable destination as a not-sent
// message. The error it returns is nil when the refusal was recorded, so the
// reply goes on to acknowledge the parent.
func (s Store) refuseReply(sender model.Session, actor string, parent model.Message, body string, target model.Session, code model.ErrorCode, refusalReason string) (msg model.Message, _ model.Session, fresh bool, err error) {
	target.ID = parent.From
	msg, err = s.outgoingMessage(sender, target, replyID(actor, parent.ID, body), body, parent.ID)
	if err == nil {
		err = msg.SettleNotSent(code, refusalReason)
	}
	if err == nil {
		err = s.chargeRefusedReply(&msg)
	}
	return msg, target, false, err
}

// chargeRefusedReply takes flood quota for a refused reply, but only for a
// record that does not exist yet: a retry of an existing record must not spend
// it again. If that fails, the message is settled again with that error's code.
// Refused reply records also grow the journal, so they must use quota.
func (s Store) chargeRefusedReply(msg *model.Message) error {
	var err error
	if _, lookupErr := s.message(msg.ID); errors.Is(lookupErr, os.ErrNotExist) {
		err = s.consumeFloodQuota(*msg, time.Now())
	} else if lookupErr != nil {
		err = lookupErr
	}
	if err != nil {
		if moveErr := msg.SettleNotSent(model.ErrorCodeOf(err), err.Error()); moveErr != nil {
			return moveErr
		}
	}
	return err
}

// commitReplyIntent writes the durable intent (the commit point) and applies
// it: reservation, acknowledgement of the parent, removal of the intent.
func (s Store) commitReplyIntent(parent, msg model.Message, fresh bool) error {
	if parent.AcknowledgedAt == "" {
		parent.AcknowledgedAt = model.Timestamp()
	}
	tx := replyTransaction{Schema: 1, ParentID: parent.ID, AcknowledgedAt: parent.AcknowledgedAt, Reply: msg}
	dir := filepath.Join(s.Dir, "transactions")
	if err := privateDir(dir); err != nil {
		return err
	}
	if err := journal.SyncDir(s.Dir); err != nil {
		return err
	}
	path := filepath.Join(dir, msg.ID+".json")
	if err := s.writeRecord(path, tx); err != nil {
		return err
	}
	s.logEvent(slog.LevelDebug, "reply.intent_committed", messageEvent(msg))
	if err := s.applyReplyTransaction(path, tx); err != nil {
		return err
	}
	level := slog.LevelDebug
	if fresh {
		level = slog.LevelInfo
	}
	s.logEvent(level, "reply.committed", messageEvent(msg))
	return nil
}

// Every journal entry point calls recovery while holding the same lock. The
// intent is the commit point: readers observe both records after recovery, or
// receive an error. Outgoing records are written before the incoming ACK.
func (s Store) recoverReplies() error {
	paths, err := filepath.Glob(filepath.Join(s.Dir, "transactions", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range paths {
		var tx replyTransaction
		if err := readJSON(path, &tx); err != nil {
			return err
		}
		if err := s.applyReplyTransaction(path, tx); err != nil {
			return err
		}
		s.logEvent(slog.LevelWarn, "reply.recovered", messageEvent(tx.Reply))
	}
	return nil
}

// applyReplyTransaction completes a stored reply intent. It checks, in this
// order: the intent itself (validateReplyTransaction: schema, UUID, identity,
// retry identity, status), the parent (it exists and matches), and any
// reservation already committed for the reply. Only then does it write the
// reservation if it is missing, acknowledge the parent and remove the intent.
func (s Store) applyReplyTransaction(path string, tx replyTransaction) error {
	if err := validateReplyTransaction(s.Dir, path, tx); err != nil {
		return err
	}
	parent, err := s.matchingReplyParent(tx)
	if err != nil {
		return err
	}
	missing, err := s.replyReservationMissing(tx)
	if err != nil {
		return err
	}
	if missing {
		if err := s.restoreReplyReservation(tx); err != nil {
			return err
		}
	}
	return s.finishReplyTransaction(path, tx, parent)
}

// validateReplyTransaction checks what the intent says about itself, in this
// order: schema, reply ID is a UUID, file name / parent / acknowledgement time,
// the ID the reply must have, and the delivery status.
func validateReplyTransaction(dir, path string, tx replyTransaction) error {
	if tx.Schema != 1 || tx.Reply.Schema < 0 || tx.Reply.Schema > recordSchema {
		return model.NewError(model.CodeUnsupportedSchema, "unsupported reply transaction schema")
	}
	if _, err := model.UUID(tx.Reply.ID); err != nil {
		return err
	}
	if path != filepath.Join(dir, "transactions", tx.Reply.ID+".json") || tx.ParentID != tx.Reply.ReplyTo || tx.AcknowledgedAt == "" {
		return model.NewError(model.CodeInvalidIdentity, "invalid reply transaction identity")
	}
	if tx.Reply.ID != replyID(tx.Reply.From, tx.ParentID, tx.Reply.Body) {
		return model.NewError(model.CodeInvalidIdentity, "reply transaction has an invalid retry identity")
	}
	if !tx.Reply.Status.Valid() {
		return model.NewError(model.CodeInvalidTransition, "invalid reply transaction status")
	}
	return nil
}

// matchingReplyParent loads the parent of the reply. It must exist, be
// addressed to the reply's sender, come from the reply's recipient, and still
// be acknowledgeable.
func (s Store) matchingReplyParent(tx replyTransaction) (model.Message, error) {
	parent, err := s.message(tx.ParentID)
	if err != nil {
		return parent, err
	}
	if parent.To != tx.Reply.From || parent.From != tx.Reply.To || !parent.Status.CanAcknowledge() {
		return parent, model.NewError(model.CodeConflict, "reply transaction does not match its parent")
	}
	return parent, nil
}

// replyReservationMissing reports whether the reply has no committed record
// yet. A record that exists must agree with the intent.
func (s Store) replyReservationMissing(tx replyTransaction) (bool, error) {
	old, err := s.message(tx.Reply.ID)
	if err == nil {
		if old.From != tx.Reply.From || old.To != tx.Reply.To || old.Body != tx.Reply.Body || old.ReplyTo != tx.ParentID || old.TargetGeneration != tx.Reply.TargetGeneration {
			return false, model.NewError(model.CodeConflict, "reply reservation conflicts with committed transaction")
		}
		return false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}

// restoreReplyReservation writes the reply record from the stored intent.
func (s Store) restoreReplyReservation(tx replyTransaction) error {
	if err := s.saveReservation(tx.Reply); err != nil {
		return err
	}
	s.logReservation(tx.Reply)
	return nil
}

// finishReplyTransaction acknowledges the parent if it is not yet, then removes
// the intent: the last step, after which the reply is fully applied.
func (s Store) finishReplyTransaction(path string, tx replyTransaction, parent model.Message) error {
	if parent.AcknowledgedAt == "" {
		parent.AcknowledgedAt = tx.AcknowledgedAt
		if err := s.saveMessage(parent); err != nil {
			return err
		}
		s.logEvent(slog.LevelInfo, "message.acknowledged", messageEvent(parent))
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return journal.SyncDir(filepath.Dir(path))
}
