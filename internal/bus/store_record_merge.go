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

// writeRecord is called under the journal lock. It writes exactly the typed
// value, so a field the value omits is not kept from an earlier record. It never
// overwrites a record it cannot trust: an existing file that cannot be read, or
// whose schema is outside 0..recordSchema, is refused and left unchanged (a
// missing schema reads as 0; readers that need the exact schema use readRecord).
func (s Store) writeRecord(path string, value any) error {
	var previous json.RawMessage
	if err := readJSON(path, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(previous) != 0 {
		if err := checkSchema(previous); err != nil {
			return err
		}
	}
	encoded, err := encodeStateJSON(value)
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

// checkSchema rejects a record whose schema is outside 0..recordSchema.
func checkSchema(raw json.RawMessage) error {
	_, err := journal.CheckSchema(raw, recordSchema)
	var schema *journal.SchemaError
	if errors.As(err, &schema) {
		return model.NewError(model.CodeUnsupportedSchema, "unsupported journal schema %d", schema.Version)
	}
	return err
}
