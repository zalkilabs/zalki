package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// messageStatusEvent projects a message write. It returns an empty event when the
// status, hold and acknowledgement are unchanged.
func messageStatusEvent(previous []byte, next model.Message) (model.Event, error) {
	projection, err := messageProjection(previous, next)
	if err != nil || projection == nil {
		return model.Event{}, err
	}
	event := model.Event{ID: newID(), Type: model.EventMessageStatusChanged, OccurredAt: model.Timestamp(), TransactionID: newID(), Message: projection}
	return event, event.Validate()
}

func messageProjection(previous []byte, next model.Message) (*model.EventMessage, error) {
	projection := &model.EventMessage{ID: next.ID, ReplyTo: next.ReplyTo, From: next.From, To: next.To, Kind: next.Kind,
		Status: next.Status, Held: next.Held, Acknowledged: next.AcknowledgedAt != ""}
	if previous == nil {
		return projection, nil
	}
	var before model.Message
	if err := json.Unmarshal(previous, &before); err != nil {
		return nil, err
	}
	if err := checkSchema(previous); err != nil {
		return nil, err
	}
	if before.ID != next.ID {
		return nil, journal.ErrInvalidBatch
	}
	projection.Previous, projection.PreviousHeld, projection.PreviousAcknowledged = before.Status, before.Held, before.AcknowledgedAt != ""
	if projection.Previous == projection.Status && projection.PreviousHeld == projection.Held && projection.PreviousAcknowledged == projection.Acknowledged {
		return nil, nil
	}
	return projection, nil
}

func validatePreparedMessage(previous []byte, next model.Message, event model.Event) error {
	projection, err := messageProjection(previous, next)
	if err != nil {
		return err
	}
	if projection == nil || event.Message == nil || *projection != *event.Message {
		return errors.New("event does not match message change")
	}
	return event.Validate()
}

// validateEventMessage checks that the message after-image matches its event.
func validateEventMessage(raw []byte, event model.Event) error {
	var next model.Message
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	if err := checkSchema(raw); err != nil {
		return err
	}
	m := event.Message
	if next.ID != m.ID || next.ReplyTo != m.ReplyTo || next.From != m.From || next.To != m.To || next.Kind != m.Kind ||
		next.Status != m.Status || next.Held != m.Held || (next.AcknowledgedAt != "") != m.Acknowledged {
		return errors.New("event does not match resulting message")
	}
	return nil
}

// writeMessage writes a message record under the journal lock. A change of
// status, hold or acknowledgement is committed with its event in one batch;
// any other change is a plain record write.
func (s Store) writeMessage(path string, next model.Message) error {
	prior, err := journal.ReadBytes(path, maxStateFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		prior, err = nil, nil
	}
	if err != nil {
		return err
	}
	if err := checkStoredPluginBinding(prior, next); err != nil {
		return err
	}
	if err := checkReplyParties(s, prior, next); err != nil {
		return err
	}
	event, err := messageStatusEvent(prior, next)
	if err != nil {
		return err
	}
	if event.ID == "" {
		return s.writeRecord(path, next)
	}
	writer := commitWriter{dir: s.Dir}
	batch, err := writer.prepareMessage(prior, next, event)
	if err != nil {
		return err
	}
	_, metadata, err := writer.readMetadata()
	if err != nil {
		return err
	}
	if err := ensureEventDirectories(s, orderEventDirectory, metadata != nil); err != nil {
		return err
	}
	if err := writer.orderSubjectDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := writer.log().Commit(batch); err != nil {
		return fmt.Errorf("message commit failed or may require recovery: %w", err)
	}
	return nil
}

// checkReplyParties keeps the invariant the plugin feed relies on: a reply goes
// between its parent's parties, reversed, so a plugin that sees a reply has
// also seen its parent. The parent is read only when a record gets a new
// reply_to.
func checkReplyParties(s Store, prior []byte, next model.Message) error {
	if next.ReplyTo == "" {
		return nil
	}
	if prior != nil {
		var before model.Message
		if err := json.Unmarshal(prior, &before); err != nil {
			return err
		}
		if before.ReplyTo == next.ReplyTo {
			return nil
		}
	}
	parent, err := s.message(next.ReplyTo)
	if err != nil {
		return err
	}
	if parent.From != next.To || parent.To != next.From {
		return model.NewError(model.CodeInvalidInput, "reply must return to the original sender")
	}
	return nil
}
