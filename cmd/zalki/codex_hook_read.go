package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
	"github.com/zalkilabs/zalki/internal/journal"
)

const codexDiagnosticFileLimit = 1 << 20

// This checker deliberately reads two user files only. A symbolic parent can
// change the persisted source key, so uncertain path spelling is not verified.
func readCodexDiagnosticFile(path string) ([]byte, string) {
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil, "missing_file"
	}
	if err != nil || parent != filepath.Dir(path) {
		return nil, "unsafe_file"
	}
	raw, err := journal.ReadBytes(path, codexDiagnosticFileLimit)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "missing_file"
	}
	if err != nil {
		return nil, "unsafe_file"
	}
	if !utf8.Valid(raw) {
		return nil, "invalid_data"
	}
	return raw, ""
}

func decodeCodexJSON(raw []byte) (map[string]any, error) {
	if !utf8.Valid(raw) || !codexValidSurrogates(raw) {
		return nil, errCodexDefinition
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	value, err := codexJSONValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errCodexDefinition
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errCodexDefinition
	}
	return object, nil
}

func codexJSONValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errCodexDefinition
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, errCodexDefinition
	}
	switch token {
	case json.Delim('{'):
		return codexJSONObject(decoder, depth)
	case json.Delim('['):
		array := []any{}
		for decoder.More() {
			value, err := codexJSONValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if end, err := decoder.Token(); err != nil || end != json.Delim(']') {
			return nil, errCodexDefinition
		}
		return array, nil
	}
	if number, ok := token.(json.Number); ok {
		if _, err := number.Int64(); err != nil {
			return nil, errCodexDefinition
		}
	}
	return token, nil
}

func codexJSONObject(decoder *json.Decoder, depth int) (map[string]any, error) {
	object := map[string]any{}
	for decoder.More() {
		token, err := decoder.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, errCodexDefinition
		}
		if _, duplicate := object[key]; duplicate {
			return nil, errCodexDefinition
		}
		value, err := codexJSONValue(decoder, depth+1)
		if err != nil {
			return nil, err
		}
		object[key] = value
	}
	if end, err := decoder.Token(); err != nil || end != json.Delim('}') {
		return nil, errCodexDefinition
	}
	return object, nil
}

// encoding/json replaces unpaired escaped surrogates with U+FFFD. Codex's
// parser rejects them; refuse that lossy conversion before token decoding.
func codexValidSurrogates(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) || raw[i] != 'u' {
			continue
		}
		unit, ok := codexHexUnit(raw, i+1)
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
		if !codexLowSurrogate(raw, i+1) {
			return false
		}
		i += 6
	}
	return true
}

func codexLowSurrogate(raw []byte, start int) bool {
	if start+5 >= len(raw) || raw[start] != '\\' || raw[start+1] != 'u' {
		return false
	}
	low, ok := codexHexUnit(raw, start+2)
	return ok && low >= 0xdc00 && low <= 0xdfff
}

func codexHexUnit(raw []byte, start int) (uint64, bool) {
	if start+4 > len(raw) {
		return 0, false
	}
	value, err := strconv.ParseUint(string(raw[start:start+4]), 16, 16)
	return value, err == nil
}

func readCodexHookStates(path string) (map[string]any, string) {
	raw, reason := readCodexDiagnosticFile(path)
	if reason != "" {
		return nil, reason
	}
	var config map[string]any
	if toml.Unmarshal(raw, &config) != nil {
		return nil, "invalid_data"
	}
	if config["hooks"] == nil {
		return map[string]any{}, ""
	}
	hooks, ok := config["hooks"].(map[string]any)
	if !ok || !codexKnownFields(hooks, "state") {
		return nil, "unsupported_definition"
	}
	if hooks["state"] == nil {
		return map[string]any{}, ""
	}
	states, ok := hooks["state"].(map[string]any)
	if !ok {
		return nil, "invalid_data"
	}
	for _, value := range states {
		if !codexValidStateShape(value) {
			return nil, "invalid_data"
		}
	}
	return states, ""
}

func codexValidStateShape(value any) bool {
	state, ok := value.(map[string]any)
	if !ok || !codexKnownFields(state, "enabled", "trusted_hash", "trusted_content_hash") {
		return false
	}
	if _, valid := codexStateEnabled(state); !valid {
		return false
	}
	for _, field := range []string{"trusted_hash", "trusted_content_hash"} {
		if value, exists := state[field]; exists {
			if _, ok := value.(string); !ok {
				return false
			}
		}
	}
	return true
}
