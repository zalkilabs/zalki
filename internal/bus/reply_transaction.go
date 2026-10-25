package bus

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

type replyTransaction struct {
	Schema         int     `json:"schema"`
	ParentID       string  `json:"parent_id"`
	AcknowledgedAt string  `json:"acknowledged_at"`
	Reply          Message `json:"reply"`
}

// Bind reply IDs to sender, parent, and body so retries reuse the reservation.
func replyID(actor, parent, body string) string {
	h := sha256.Sum256([]byte("zalki/reply/v1\x00" + actor + "\x00" + parent + "\x00" + body))
	h[6], h[8] = h[6]&15|128, h[8]&63|128
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

func (s Store) prepareReply(request ReplyRequest) (msg Message, target Session, fresh bool, err error) {
	actor, key, id, body := request.Actor, request.Capability, request.MessageID, request.Body
	err = s.locked(func() error {
		sender, err := s.authenticate(actor, key)
		if err != nil {
			return err
		}
		parent, err := s.message(id)
		if err != nil {
			return err
		}
		if parent.To != actor {
			return fail(CodeUnauthorized, "only the bound recipient can reply")
		}
		if !parent.Status.canAcknowledge() {
			return fail(CodeInvalidTransition, "cannot acknowledge a message that was not sent")
		}
		msg, target, fresh, err = s.prepareMessage(SendRequest{Actor: actor, Capability: key, To: parent.From, MessageID: replyID(actor, parent.ID, body), Body: body, ReplyTo: parent.ID}, false)
		if code := ErrorCodeOf(err); code == CodeUnavailable || code == CodeNotFound {
			refusal := &Receipt{At: timestamp(), Status: StatusNotSent, Code: code, Error: err.Error()}
			target.ID = parent.From
			msg, err = s.outgoingMessage(sender, target, replyID(actor, parent.ID, body), body, parent.ID)
			msg.Status, msg.Receipt, fresh = StatusNotSent, refusal, false
		}
		if err != nil {
			return err
		}
		if parent.AcknowledgedAt == "" {
			parent.AcknowledgedAt = timestamp()
		}
		tx := replyTransaction{Schema: 1, ParentID: parent.ID, AcknowledgedAt: parent.AcknowledgedAt, Reply: msg}
		dir := filepath.Join(s.Dir, "transactions")
		if err := privateDir(dir); err != nil {
			return err
		}
		if err := fsutil.SyncDir(s.Dir); err != nil {
			return err
		}
		path := filepath.Join(dir, msg.ID+".json")
		if err := writeJSON(path, tx); err != nil {
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
	})
	return
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

func (s Store) applyReplyTransaction(path string, tx replyTransaction) error {
	if tx.Schema != 1 || tx.Reply.Schema < 0 || tx.Reply.Schema > recordSchema {
		return fail(CodeUnsupportedSchema, "unsupported reply transaction schema")
	}
	if _, err := UUID(tx.Reply.ID); err != nil {
		return err
	}
	if path != filepath.Join(s.Dir, "transactions", tx.Reply.ID+".json") || tx.ParentID != tx.Reply.ReplyTo || tx.AcknowledgedAt == "" {
		return fail(CodeInvalidIdentity, "invalid reply transaction identity")
	}
	if tx.Reply.ID != replyID(tx.Reply.From, tx.ParentID, tx.Reply.Body) {
		return fail(CodeInvalidIdentity, "reply transaction has an invalid retry identity")
	}
	if !tx.Reply.Status.valid() {
		return fail(CodeInvalidTransition, "invalid reply transaction status")
	}
	parent, err := s.message(tx.ParentID)
	if err != nil {
		return err
	}
	if parent.To != tx.Reply.From || parent.From != tx.Reply.To || !parent.Status.canAcknowledge() {
		return fail(CodeConflict, "reply transaction does not match its parent")
	}
	old, err := s.message(tx.Reply.ID)
	if err == nil {
		if old.From != tx.Reply.From || old.To != tx.Reply.To || old.Body != tx.Reply.Body || old.ReplyTo != tx.ParentID || old.TargetGeneration != tx.Reply.TargetGeneration {
			return fail(CodeConflict, "reply reservation conflicts with committed transaction")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		if err := s.saveMessage(tx.Reply); err != nil {
			return err
		}
		s.logReservation(tx.Reply)
	}
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
	return fsutil.SyncDir(filepath.Dir(path))
}
