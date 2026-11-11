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
// from a durable reply intent. Session writes are prepared by commitWriter.
type recordUpdate struct {
	previous json.RawMessage
}

// writeRecord is called under the journal lock. Re-read the on-disk record so
// re-registration and crash recovery also preserve fields absent from their
// newly constructed values. A previous record whose schema is outside
// 0..supported is never overwritten (a missing schema reads as 0 and is
// accepted here; readers that need the exact schema use readRecord).
func (s Store) writeRecord(path string, value any, supported int, updates ...recordUpdate) error {
	update := recordUpdate{}
	if len(updates) != 0 {
		update = updates[0]
	}
	var previous json.RawMessage
	if err := readJSON(path, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(previous) == 0 {
		previous = update.previous
	}
	if len(previous) != 0 {
		if err := checkSchema(previous, supported); err != nil {
			return err
		}
	}
	raw, err := mergeRecordJSON(previous, value)
	if err != nil {
		return err
	}
	// Unknown fields are never dropped to make an oversized merge fit.
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
	return journal.WriteAtomic(path, encoded)
}

// checkSchema rejects a record whose schema this build does not support.
func checkSchema(raw json.RawMessage, supported int) error {
	_, err := journal.CheckSchema(raw, supported)
	var schema *journal.SchemaError
	if errors.As(err, &schema) {
		return model.NewError(model.CodeUnsupportedSchema, "unsupported journal schema %d", schema.Version)
	}
	return err
}
