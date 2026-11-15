package journal

import (
	"encoding/json"
	"fmt"
)

// SchemaError reports a record whose schema does not match the expected version.
type SchemaError struct {
	Version, Supported int
}

func (e *SchemaError) Error() string {
	return fmt.Sprintf("unsupported record schema %d; expected %d", e.Version, e.Supported)
}

// CheckSchema returns the "schema" version in the JSON object raw. A missing
// field is version 0. A version other than supported is a *[SchemaError];
// malformed JSON returns the decoder's error.
func CheckSchema(raw json.RawMessage, supported int) (int, error) {
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return 0, err
	}
	if header.Schema != supported {
		return 0, &SchemaError{Version: header.Schema, Supported: supported}
	}
	return header.Schema, nil
}
