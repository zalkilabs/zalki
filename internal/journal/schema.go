package journal

import (
	"encoding/json"
	"fmt"
)

// SchemaError reports a record whose schema version is outside what the
// caller supports.
type SchemaError struct {
	Version, Supported int
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("unsupported record schema %d; supported up to %d", e.Version, e.Supported)
}

// CheckSchema returns the "schema" version in the JSON object raw. A missing
// field is version 0. A version below zero or above supported is a
// *[SchemaError]; malformed JSON returns the decoder's error.
func CheckSchema(raw json.RawMessage, supported int) (int, error) {
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return 0, err
	}
	if header.Schema < 0 || header.Schema > supported {
		return 0, &SchemaError{Version: header.Schema, Supported: supported}
	}
	return header.Schema, nil
}
