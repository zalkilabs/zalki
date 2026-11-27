package install

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

// errStrictRecord is every refusal of the strict record reader.
var errStrictRecord = errors.New("unsupported installation record")

// RecordedTargets tells a diagnostic which file the installation record of this
// home and state directory holds for each declared file path. It is a pure
// function over the record's bytes: the caller reads the file safely and passes
// the bytes, or the reason it could not (readReason, returned unchanged:
// "missing_file", "unsafe_file" or "invalid_data"). The reason is "" when the
// record belongs to this installation, "invalid_data" when the bytes are not
// one strict JSON object (a duplicate key, an unpaired surrogate, nesting deeper
// than 64 or a number that is not a 64-bit integer are refused), and
// "ambiguous_ownership" when it is for another schema, home or state directory
// or has no targets.
func RecordedTargets(raw []byte, readReason, home, stateDir string) (map[string]string, string) {
	if readReason != "" {
		return nil, readReason
	}
	record, err := decodeStrictRecord(raw)
	if err != nil {
		return nil, "invalid_data"
	}
	schema, isNumber := record["schema"].(json.Number)
	if value, err := schema.Int64(); !isNumber || err != nil || value != 3 || record["home"] != home || record["state"] != stateDir {
		return nil, "ambiguous_ownership"
	}
	targets, ok := record["targets"].(map[string]any)
	if !ok {
		return nil, "ambiguous_ownership"
	}
	recorded := make(map[string]string, len(targets))
	for key, value := range targets {
		if path, isPath := value.(string); isPath {
			recorded[key] = path
		}
	}
	return recorded, ""
}

// The decoder below is a copy of the one the Codex hook-trust engine uses for
// its own files (internal/adapters/codex/trust_read.go): the same rules, so that
// the record is read as strictly as it was before the engine moved.

func decodeStrictRecord(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) || !validRecordSurrogates(raw) {
		return nil, errStrictRecord
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := strictRecordValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errStrictRecord
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errStrictRecord
	}
	return object, nil
}

func strictRecordValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errStrictRecord
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, errStrictRecord
	}
	switch token {
	case json.Delim('{'):
		return strictRecordObject(decoder, depth)
	case json.Delim('['):
		array := []any{}
		for decoder.More() {
			value, err := strictRecordValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return nil, errStrictRecord
		}
		return array, nil
	}
	if number, ok := token.(json.Number); ok {
		if _, err := number.Int64(); err != nil {
			return nil, errStrictRecord
		}
	}
	return token, nil
}

func strictRecordObject(decoder *json.Decoder, depth int) (map[string]any, error) {
	object := map[string]any{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errStrictRecord
		}
		if _, duplicate := object[key]; duplicate {
			return nil, errStrictRecord
		}
		value, err := strictRecordValue(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		object[key] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, errStrictRecord
	}
	return object, nil
}

// encoding/json replaces unpaired escaped surrogates with U+FFFD. Codex's
// parser rejects them; refuse that lossy conversion before token decoding.
func validRecordSurrogates(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		unit, ok := hexUnit(raw, i+1)
		if !ok {
			return false
		}
		i += 4
		if unit >= 0xdc00 && unit <= 0xdfff {
			return false
		}
		if unit < 0xd800 || unit > 0xdbff {
			continue
		}
		if !lowSurrogate(raw, i+1) {
			return false
		}
		i += 6
	}
	return true
}

func lowSurrogate(raw []byte, start int) bool {
	if start+5 >= len(raw) || raw[start] != '\\' || raw[start+1] != 'u' {
		return false
	}
	low, ok := hexUnit(raw, start+2)
	return ok && low >= 0xdc00 && low <= 0xdfff
}

func hexUnit(raw []byte, start int) (uint64, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	value, err := strconv.ParseUint(string(raw[start:start+4]), 16, 16)
	return value, err == nil
}
