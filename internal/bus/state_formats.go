package bus

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

const stateFormatFile = "state-formats.json"

type stateFormats struct {
	Schema  int            `json:"schema"`
	Formats map[string]int `json:"formats"`
}

var supportedStateFormats = map[string]int{
	"sessions":     leaseSessionSchema,
	"messages":     recordSchema,
	"watches":      recordSchema,
	"flood":        recordSchema,
	"transactions": recordSchema,
	"runtimes":     recordSchema,
}

type stateFormatError struct{ cause error }

func (e *stateFormatError) Error() string { return e.cause.Error() }
func (e *stateFormatError) Unwrap() error { return e.cause }

// IsUpgradeRequired identifies the state-wide write fence, distinct from an
// individual unreadable record. Read-only commands can keep serving known data.
func IsUpgradeRequired(err error) bool {
	var format *stateFormatError
	return errors.As(err, &format)
}

func formatWriteError() error {
	return &stateFormatError{cause: fail(CodeUnsupportedSchema, "state formats are unsupported or unreadable; upgrade zalki before writing")}
}

func (s Store) readStateFormats() (stateFormats, json.RawMessage, error) {
	var marker stateFormats
	var raw json.RawMessage
	if err := readJSON(filepath.Join(s.Dir, stateFormatFile), &raw); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stateFormats{Schema: 1, Formats: map[string]int{}}, nil, nil
		}
		return marker, nil, formatWriteError()
	}
	// Existing files must contain explicit canonical fields. Defaults belong
	// only to the missing-file path; otherwise null or absent fields hide damage.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return marker, nil, formatWriteError()
	}
	if err := json.Unmarshal(fields["schema"], &marker.Schema); err != nil || marker.Schema != 1 {
		return marker, nil, formatWriteError()
	}
	if err := json.Unmarshal(fields["formats"], &marker.Formats); err != nil || marker.Formats == nil {
		return marker, nil, formatWriteError()
	}
	for family, version := range marker.Formats {
		supported, exists := supportedStateFormats[family]
		if !exists || version < 1 || version > supported {
			return marker, nil, formatWriteError()
		}
	}
	return marker, raw, nil
}

// CheckWriteCompatibility is read-only. Hooks use it before capture, owner
// updates or adapter callbacks, so an incompatible shared state cannot break a
// client lifecycle hook or produce partial startup output.
func (s Store) CheckWriteCompatibility() error {
	_, _, err := s.readStateFormats()
	return err
}

// ensureStateFormat publishes a monotonic high-water mark before the record.
// It is lazy: an empty/read-only store gains no marker. Older builds ignore this
// file, and records already written before markers still use per-record checks.
// Caller holds the journal lock. A failed record write may leave a conservative
// format declaration, but never an undeclared newer record.
func (s Store) ensureStateFormat(family string, version int) error {
	marker, raw, err := s.readStateFormats()
	if err != nil {
		return err
	}
	supported, known := supportedStateFormats[family]
	if !known || version < 1 || version > supported {
		return formatWriteError()
	}
	if marker.Formats[family] >= version {
		return nil
	}
	marker.Formats[family] = version
	merged, err := mergeRecordJSON(raw, marker)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(s.Dir, stateFormatFile), merged)
}
