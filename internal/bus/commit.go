package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// commitWriter publishes a prepared session or message record with its event
// under the Store lock. Its caller owns directory durability.
type commitWriter struct {
	dir          string
	limit        uint64
	orderSubject func(string) error
}

func (w commitWriter) log() journal.BatchLog {
	return journal.BatchLog{Root: w.dir, Intent: "commits/pending.json"}
}

// previous must be the exact on-disk bytes, including surrounding whitespace;
// decoding into json.RawMessage is not an exact-byte read for this purpose.
func (w commitWriter) prepare(previous []byte, next model.Session, event model.Event) (journal.Batch, error) {
	if err := validatePreparedSession(previous, next, event); err != nil {
		return journal.Batch{}, err
	}
	state, err := encodeStateJSON(next)
	if err != nil {
		return journal.Batch{}, err
	}
	return w.prepareSubject(previous, state, event)
}

// prepareMessage builds the batch for a message write and its event.
func (w commitWriter) prepareMessage(previous []byte, next model.Message, event model.Event) (journal.Batch, error) {
	if err := validatePreparedMessage(previous, next, event); err != nil {
		return journal.Batch{}, err
	}
	state, err := encodeStateJSON(next)
	if err != nil {
		return journal.Batch{}, err
	}
	return w.prepareSubject(previous, state, event)
}

func (w commitWriter) prepareSubject(previous, state []byte, event model.Event) (journal.Batch, error) {
	batch, err := w.prepareEvents(previous, state, event)
	if err != nil {
		return journal.Batch{}, err
	}
	if err := w.validate(batch); err != nil {
		return journal.Batch{}, err
	}
	return batch, nil
}

func (w commitWriter) orderSubjectDirectory(path string) error {
	if w.orderSubject != nil {
		return w.orderSubject(path)
	}
	return journal.OrderDirectory(path)
}

func (w commitWriter) commit(previous []byte, next model.Session, event model.Event) error {
	batch, err := w.prepare(previous, next, event)
	if err != nil {
		return err
	}
	return w.log().Commit(batch)
}

func (w commitWriter) recover() error { return w.log().Recover(w.validate) }

func (w commitWriter) validate(batch journal.Batch) error {
	if len(batch.Puts) != 3 || len(batch.Puts[1].Data) > maxEventBytes || !json.Valid(batch.Puts[1].Data) {
		return journal.ErrInvalidBatch
	}
	var record eventRecord
	decoder := json.NewDecoder(bytes.NewReader(batch.Puts[1].Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return err
	}
	if err := record.validate(); err != nil {
		return err
	}
	event := record.Event
	if err := w.validateMetadata(batch, record); err != nil {
		return err
	}
	if batch.ID != event.TransactionID || batch.Puts[0].Path != eventSubjectPath(event) || batch.Puts[1].Path != eventRecordPath(record.Sequence) {
		return journal.ErrInvalidBatch
	}
	if event.Message != nil {
		return validateEventMessage(batch.Puts[0].Data, event)
	}
	return validateEventSession(batch.Puts[0].Data, event)
}

func validateEventSession(raw []byte, event model.Event) error {
	var next model.Session
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	if err := checkSchema(raw); err != nil {
		return err
	}
	if next.ID != event.Session.ID || next.Generation != event.Session.Generation || next.State != event.Session.State || next.Retired != event.Session.Retired {
		return errors.New("event does not match resulting session")
	}
	return nil
}

// eventSubjectPath is the record an event describes: its message or its session.
func eventSubjectPath(event model.Event) string {
	if event.Message != nil {
		return filepath.Join("messages", event.Message.ID+".json")
	}
	return filepath.Join("sessions", strings.ReplaceAll(event.Session.ID, ":", "_")+".json")
}
