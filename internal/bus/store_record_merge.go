package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// mergeRecordJSON preserves fields outside the typed record while replacing
// every field the type owns, including fields omitted because they were cleared.
func mergeRecordJSON(previous json.RawMessage, value any) (json.RawMessage, error) {
	return journal.Merge(previous, value)
}

// recordUpdate carries preservation data only for an exceptional first write
// from a durable reply intent. Session migration is prepared by commitWriter.
type recordUpdate struct {
	previous json.RawMessage
}

// writeRecord is called under the journal lock. Re-read the on-disk record so
// re-registration and crash recovery also preserve fields absent from their
// newly constructed values. A future record is never overwritten by a fresh value.
func (s Store) writeRecord(path string, value any, supported int, updates ...recordUpdate) error {
	update := recordUpdate{}
	if len(updates) != 0 {
		update = updates[0]
	}
	next, err := json.Marshal(value)
	if err != nil {
		return err
	}
	nextSchema, err := checkSchema(next, supported)
	if err != nil {
		return err
	}
	var previous json.RawMessage
	if err := readJSON(path, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(previous) == 0 {
		previous = update.previous
	}
	if len(previous) != 0 {
		if _, err := checkSchema(previous, supported); err != nil {
			return err
		}
	}
	raw, err := mergeRecordJSON(previous, value)
	if err != nil {
		return err
	}
	// Validate before publishing the marker, then write these exact bytes after
	// the marker. Unknown fields are never dropped to make an oversized merge fit.
	encoded, err := encodeStateJSON(raw)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(s.Dir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return model.NewError(model.CodeInvalidIdentity, "record path is outside state directory")
	}
	family, _, _ := strings.Cut(relative, string(os.PathSeparator))
	if family == "sessions" {
		return model.NewError(model.CodeInvalidInput, "session writes require the commit path")
	}
	version := nextSchema
	if version == 0 {
		version = 1
	}
	if err := s.ensureStateFormat(family, version); err != nil {
		return err
	}
	return journal.WriteAtomic(path, encoded)
}

// checkSchema returns the record's schema version and rejects one this build
// does not support.
func checkSchema(raw json.RawMessage, supported int) (int, error) {
	version, err := journal.CheckSchema(raw, supported)
	var schema *journal.SchemaError
	if errors.As(err, &schema) {
		return 0, model.NewError(model.CodeUnsupportedSchema, "unsupported journal schema %d; upgrade zalki", schema.Version)
	}
	return version, err
}

// dropFields removes obsolete fields, matched case-insensitively, from a record.
func dropFields(record json.RawMessage, removed []string) (json.RawMessage, error) {
	return journal.DropFields(record, removed)
}

// futureRecord is used only to protect cleanup candidates. Corrupt old records
// keep their existing cleanup policy, but a readable future header is kept.
func futureRecord(path string, supported int) bool {
	var header struct {
		Schema int `json:"schema"`
	}
	return readJSON(path, &header) == nil && header.Schema > supported
}
