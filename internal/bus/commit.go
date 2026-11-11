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

// commitWriter publishes prepared state and event records under the Store lock.
// Its caller owns directory durability and session validation.
type commitWriter struct {
	dir          string
	limit        uint64
	orderSession func(string) error
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
	state, err := prepareSessionBytes(previous, next)
	if err != nil {
		return journal.Batch{}, err
	}
	batch, err := w.prepareEvents(previous, state, event)
	if err != nil {
		return journal.Batch{}, err
	}
	if err := w.validate(batch); err != nil {
		return journal.Batch{}, err
	}
	return batch, nil
}

func prepareSessionBytes(previous []byte, next model.Session) ([]byte, error) {
	raw, err := mergeRecordJSON(json.RawMessage(previous), next)
	if err != nil {
		return nil, err
	}
	return encodeStateJSON(raw)
}

func (w commitWriter) orderSessionDirectory(path string) error {
	if w.orderSession != nil {
		return w.orderSession(path)
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
	if batch.ID != event.TransactionID || batch.Puts[0].Path != eventSessionPath(event) || batch.Puts[1].Path != eventRecordPath(record.Sequence) {
		return journal.ErrInvalidBatch
	}
	return validateEventSession(batch.Puts[0].Data, event)
}

func validateEventSession(raw []byte, event model.Event) error {
	var next model.Session
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	if err := checkSchema(raw, leaseSessionSchema); err != nil {
		return err
	}
	if next.ID != event.Session.ID || next.Generation != event.Session.Generation || next.State != event.Session.State || next.Retired != event.Session.Retired {
		return errors.New("event does not match resulting session")
	}
	return nil
}

func eventSessionPath(event model.Event) string {
	return filepath.Join("sessions", strings.ReplaceAll(event.Session.ID, ":", "_")+".json")
}
