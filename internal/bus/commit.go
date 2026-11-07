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
// Its caller owns format fences, directory durability and session validation.
type commitWriter struct {
	dir   string
	limit uint64
}

func (w commitWriter) log() journal.BatchLog {
	return journal.BatchLog{Root: w.dir, Intent: "commits/pending.json"}
}

// previous must be the exact on-disk bytes, including surrounding whitespace;
// decoding into json.RawMessage is not an exact-byte read for this purpose.
func (w commitWriter) prepare(previous []byte, next Session, event model.Event, removed []string) (journal.Batch, error) {
	if err := event.Validate(); err != nil {
		return journal.Batch{}, err
	}
	var before Session
	var err error
	if err := json.Unmarshal(previous, &before); err != nil {
		return journal.Batch{}, err
	}
	if before.ID != next.ID || before.Generation != next.Generation || before.State != event.Session.Previous {
		return journal.Batch{}, errors.New("event does not match previous session")
	}
	if _, err := checkSchema(previous, leaseSessionSchema); err != nil {
		return journal.Batch{}, err
	}
	mergePrior := json.RawMessage(previous)
	if len(removed) != 0 {
		mergePrior, err = dropFields(mergePrior, removed)
		if err != nil {
			return journal.Batch{}, err
		}
	}
	raw, err := mergeRecordJSON(mergePrior, next)
	if err != nil {
		return journal.Batch{}, err
	}
	state, err := encodeStateJSON(raw)
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

func (w commitWriter) commit(previous []byte, next Session, event model.Event, removed []string) error {
	batch, err := w.prepare(previous, next, event, removed)
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
	var next Session
	if err := json.Unmarshal(raw, &next); err != nil {
		return err
	}
	if _, err := checkSchema(raw, leaseSessionSchema); err != nil {
		return err
	}
	if next.ID != event.Session.ID || next.Generation != event.Session.Generation || next.State != event.Session.State {
		return errors.New("event does not match resulting session")
	}
	return nil
}

func eventSessionPath(event model.Event) string {
	return filepath.Join("sessions", strings.ReplaceAll(event.Session.ID, ":", "_")+".json")
}
