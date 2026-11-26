package statefile

import (
	"bytes"
	"encoding/json"
)

// DecodeStrictRecordJSON accepts a single value without unknown fields. Input
// that is not valid JSON returns [ErrInvalidData] itself; unknown fields and
// type errors return the decoder's own error.
func DecodeStrictRecordJSON(raw []byte, target any) error {
	if !json.Valid(raw) {
		return ErrInvalidData
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(target)
}
