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

// commitWriter is dormant: existing Store entry points do not call it. Its
// caller owns the Store lock and format fences. PR2 must integrate those
// prerequisites and existing session validation before any live mutation.
type commitWriter struct{ dir string }

func (w commitWriter) log() journal.BatchLog {
	return journal.BatchLog{Root: w.dir, Intent: "commits/pending.json"}
}

// previous must be the exact on-disk bytes, including surrounding whitespace;
// decoding into json.RawMessage is not an exact-byte read for this purpose.
func (w commitWriter) commit(previous []byte, next Session, event model.Event) error {
	if err := event.Validate(); err != nil {
		return err
	}
	var before Session
	if err := json.Unmarshal(previous, &before); err != nil {
		return err
	}
	if before.ID != next.ID || before.Generation != next.Generation || before.State != event.Session.Previous {
		return errors.New("event does not match previous session")
	}
	if _, err := checkSchema(previous, leaseSessionSchema); err != nil {
		return err
	}
	raw, err := mergeRecordJSON(previous, next)
	if err != nil {
		return err
	}
	state, err := encodeStateJSON(raw)
	if err != nil {
		return err
	}
	metadata, err := journal.EncodeJSON(event, 4096)
	if err != nil {
		return err
	}
	batch := journal.Batch{Schema: 1, ID: event.TransactionID, Puts: []journal.Put{
		journal.NewPut(eventSessionPath(event), previous, state),
		journal.NewPut(eventRecordPath(event), nil, metadata),
	}}
	if err := w.validate(batch); err != nil {
		return err
	}
	return w.log().Commit(batch)
}

func (w commitWriter) recover() error { return w.log().Recover(w.validate) }

func (w commitWriter) validate(batch journal.Batch) error {
	if len(batch.Puts) != 2 || len(batch.Puts[1].Data) > 4096 || !json.Valid(batch.Puts[1].Data) {
		return journal.ErrInvalidBatch
	}
	var event model.Event
	decoder := json.NewDecoder(bytes.NewReader(batch.Puts[1].Data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return err
	}
	if err := event.Validate(); err != nil {
		return err
	}
	if batch.ID != event.TransactionID || batch.Puts[0].Path != eventSessionPath(event) || batch.Puts[1].Path != eventRecordPath(event) {
		return journal.ErrInvalidBatch
	}
	var next Session
	if err := json.Unmarshal(batch.Puts[0].Data, &next); err != nil {
		return err
	}
	if _, err := checkSchema(batch.Puts[0].Data, leaseSessionSchema); err != nil {
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

func eventRecordPath(event model.Event) string {
	return filepath.Join("events", "records", event.ID+".json")
}
