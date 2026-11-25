package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/statefile"
)

// writeRecord is called under the journal lock. It writes exactly the typed
// value, so a field the value omits is not kept from an earlier record. It never
// overwrites a record it cannot trust: an existing file that cannot be read, or
// whose schema is not recordSchema, is refused and left unchanged. A missing
// schema reads as 0 and is refused, just as it is by readRecord.
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
	relative, err := filepath.Rel(s.dir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return model.NewError(model.CodeInvalidIdentity, "record path is outside state directory")
	}
	family, _, _ := strings.Cut(relative, string(os.PathSeparator))
	if family == "sessions" {
		return model.NewError(model.CodeInvalidInput, "session writes require the commit path")
	}
	return fsutil.WriteAtomic(path, encoded)
}

// checkSchema rejects a record whose schema is not recordSchema.
func checkSchema(raw json.RawMessage) error {
	_, err := statefile.CheckSchema(raw, recordSchema)
	var schema *statefile.SchemaError
	if errors.As(err, &schema) {
		return model.NewError(model.CodeUnsupportedSchema, "unsupported journal schema %d", schema.Version)
	}
	return err
}
