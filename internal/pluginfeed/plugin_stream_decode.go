package pluginfeed

import (
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"

	"github.com/zalkilabs/zalki/internal/statefile"
)

func decodeStreamJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := uniqueStreamValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return statefile.ErrInvalidFile
	}
	if err := exactStreamShape(raw, reflect.TypeOf(target).Elem(), 0); err != nil {
		return err
	}
	return statefile.DecodeStrictRecordJSON(raw, target)
}

func uniqueStreamValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return statefile.ErrInvalidFile
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return statefile.ErrInvalidFile
	}
	keys := map[string]bool{}
	for decoder.More() {
		if delimiter == '{' {
			if err := uniqueStreamKey(decoder, keys); err != nil {
				return err
			}
		}
		if err := uniqueStreamValue(decoder, depth+1); err != nil {
			return err
		}
	}
	end, err := decoder.Token()
	if err != nil || (delimiter == '{' && end != json.Delim('}')) || (delimiter == '[' && end != json.Delim(']')) {
		return statefile.ErrInvalidFile
	}
	return nil
}

func uniqueStreamKey(decoder *json.Decoder, keys map[string]bool) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	key, ok := token.(string)
	if !ok || keys[key] {
		return statefile.ErrInvalidFile
	}
	keys[key] = true
	return nil
}

func exactStreamShape(raw []byte, kind reflect.Type, depth int) error {
	if depth > 64 {
		return statefile.ErrInvalidFile
	}
	if kind == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	shape := kind.Kind()
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nullableStreamShape(shape)
	}
	if shape == reflect.Pointer {
		return exactStreamShape(raw, kind.Elem(), depth+1)
	}
	if shape == reflect.Struct {
		return exactStreamObject(raw, kind, depth)
	}
	if shape == reflect.Slice || shape == reflect.Array {
		return exactStreamArray(raw, kind.Elem(), depth)
	}
	if shape == reflect.Map {
		return exactStreamMap(raw, kind.Elem(), depth)
	}
	return nil
}

func nullableStreamShape(kind reflect.Kind) error {
	if kind == reflect.Pointer || kind == reflect.Map || kind == reflect.Slice {
		return nil
	}
	return statefile.ErrInvalidFile
}

func exactStreamArray(raw []byte, element reflect.Type, depth int) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	for _, item := range items {
		if err := exactStreamShape(item, element, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func exactStreamMap(raw []byte, element reflect.Type, depth int) error {
	var items map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	for _, item := range items {
		if err := exactStreamShape(item, element, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func exactStreamObject(raw []byte, kind reflect.Type, depth int) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return err
	}
	fields := map[string]reflect.StructField{}
	for i := 0; i < kind.NumField(); i++ {
		field := kind.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "" {
			name = field.Name
		}
		if name != "-" {
			fields[name] = field
		}
	}
	for name, value := range object {
		field, ok := fields[name]
		if !ok {
			return statefile.ErrInvalidFile
		}
		if name == "sequence" {
			var text string
			if json.Unmarshal(value, &text) != nil {
				return statefile.ErrInvalidFile
			}
			number, err := strconv.ParseUint(text, 10, 64)
			if err != nil || strconv.FormatUint(number, 10) != text {
				return statefile.ErrInvalidFile
			}
			continue
		}
		if err := exactStreamShape(value, field.Type, depth+1); err != nil {
			return err
		}
	}
	return nil
}
