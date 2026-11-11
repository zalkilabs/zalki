package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const (
	maxEventBytes     = 4096
	maxRetainedEvents = 4096
	eventMetaPath     = "events/meta.json"
)

type eventRecord struct {
	Epoch    string      `json:"epoch"`
	Sequence uint64      `json:"sequence,string"`
	Event    model.Event `json:"event"`
}

type eventMetadata struct {
	Epoch string `json:"epoch"`
	First uint64 `json:"first_sequence,string"`
	Last  uint64 `json:"last_sequence,string"`
	Count uint64 `json:"count"`
	Bytes uint64 `json:"bytes"`
}

func (w commitWriter) eventLimit() uint64 {
	if w.limit == 0 || w.limit > maxRetainedEvents {
		return maxRetainedEvents
	}
	return w.limit
}

func (r eventRecord) validate() error {
	if !canonicalEventID(r.Epoch) || r.Sequence == 0 {
		return journal.ErrInvalidBatch
	}
	return r.Event.Validate()
}

func canonicalEventID(id string) bool {
	return model.IsCanonicalUUID(id)
}

func (m eventMetadata) validate(limit uint64) error {
	if !canonicalEventID(m.Epoch) || m.First == 0 || m.Last < m.First {
		return journal.ErrInvalidBatch
	}
	if m.Count == 0 || m.Count > limit || m.Count != m.Last-m.First+1 || m.Bytes == 0 || m.Bytes > m.Count*maxEventBytes {
		return journal.ErrInvalidBatch
	}
	return nil
}

func decodeEventJSON(raw []byte, target any) error {
	return DecodeStrictRecordJSON(raw, target)
}

// DecodeStrictRecordJSON accepts a single value without unknown fields.
func DecodeStrictRecordJSON(raw []byte, target any) error {
	if !json.Valid(raw) {
		return journal.ErrInvalidBatch
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}

func eventRecordPath(sequence uint64) string {
	return filepath.Join("events", "records", fmt.Sprintf("%020d.json", sequence))
}

func (w commitWriter) readMetadata() (eventMetadata, []byte, error) {
	raw, err := journal.ReadBytes(filepath.Join(w.dir, eventMetaPath), maxStateFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		if err := w.checkEmptyRecords(); err != nil {
			return eventMetadata{}, nil, err
		}
		return eventMetadata{Epoch: newID(), First: 1}, nil, nil
	}
	if err != nil {
		return eventMetadata{}, nil, err
	}
	var metadata eventMetadata
	if err := decodeEventJSON(raw, &metadata); err != nil {
		return metadata, nil, err
	}
	return metadata, raw, metadata.validate(w.eventLimit())
}

func (w commitWriter) checkEmptyRecords() error {
	dir, err := os.Open(filepath.Join(w.dir, "events", "records"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck // Read-only directory descriptor.
	names, err := dir.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) != 0 {
		return journal.ErrInvalidBatch
	}
	return nil
}

func (w commitWriter) retireOldest(metadata *eventMetadata) ([]journal.Delete, error) {
	if metadata.Count < w.eventLimit() {
		return nil, nil
	}
	path := eventRecordPath(metadata.First)
	raw, err := journal.ReadBytes(filepath.Join(w.dir, path), maxEventBytes)
	if err != nil {
		return nil, err
	}
	var oldest eventRecord
	if err := decodeEventJSON(raw, &oldest); err != nil {
		return nil, err
	}
	if err := oldest.validate(); err != nil {
		return nil, err
	}
	if oldest.Sequence != metadata.First || oldest.Epoch != metadata.Epoch || metadata.Bytes < uint64(len(raw)) {
		return nil, journal.ErrInvalidBatch
	}
	metadata.First++
	metadata.Count--
	metadata.Bytes -= uint64(len(raw))
	return []journal.Delete{journal.NewDelete(path, raw)}, nil
}

func (w commitWriter) prepareEvents(previous, state []byte, event model.Event) (journal.Batch, error) {
	metadata, priorMetadata, err := w.readMetadata()
	if err != nil {
		return journal.Batch{}, err
	}
	if metadata.Last == math.MaxUint64 {
		return journal.Batch{}, errors.New("event sequence exhausted")
	}
	deletes, err := w.retireOldest(&metadata)
	if err != nil {
		return journal.Batch{}, err
	}
	metadata.Last++
	record := eventRecord{Epoch: metadata.Epoch, Sequence: metadata.Last, Event: event}
	raw, err := journal.EncodeJSON(record, maxEventBytes)
	if err != nil {
		return journal.Batch{}, err
	}
	metadata.Count++
	metadata.Bytes += uint64(len(raw))
	metaBytes, err := encodeStateJSON(metadata)
	if err != nil {
		return journal.Batch{}, err
	}
	return journal.Batch{ID: event.TransactionID, Puts: []journal.Put{
		journal.NewPut(eventSubjectPath(event), previous, state),
		journal.NewPut(eventRecordPath(record.Sequence), nil, raw),
		journal.NewPut(eventMetaPath, priorMetadata, metaBytes),
	}, Deletes: deletes}, nil
}

func (w commitWriter) validateMetadata(batch journal.Batch, record eventRecord) error {
	if batch.Puts[2].Path != eventMetaPath || len(batch.Deletes) > 1 {
		return journal.ErrInvalidBatch
	}
	var metadata eventMetadata
	if err := decodeEventJSON(batch.Puts[2].Data, &metadata); err != nil {
		return err
	}
	if err := metadata.validate(w.eventLimit()); err != nil {
		return err
	}
	if metadata.Epoch != record.Epoch || metadata.Last != record.Sequence {
		return journal.ErrInvalidBatch
	}
	if len(batch.Deletes) != 0 && (metadata.First <= 1 || batch.Deletes[0].Path != eventRecordPath(metadata.First-1)) {
		return journal.ErrInvalidBatch
	}
	return nil
}
