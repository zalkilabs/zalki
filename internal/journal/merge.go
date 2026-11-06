package journal

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// Merge returns the JSON encoding of value combined with the previous encoding
// of the same record. Fields that value's type owns come from value, including
// fields it omitted because they were cleared. Fields the type does not own
// come from previous, so a build that does not know a field still preserves it.
//
// For ordinary exported fields, ownership uses the json tag name
// (or Go name), matched case-insensitively, and nested structs, string-keyed
// maps and slices of structs with an "id" field are merged the same way.
// Entries of a map that are absent from value are removed. Values with a
// custom JSON encoding, [json.RawMessage] values and other slices are replaced
// whole. An empty or null previous value, or a typed nil value, returns
// value's encoding unchanged. The value must carry a concrete Go type; untyped
// nil is not supported. This preserves the existing typed-record policy, not
// a complete implementation of encoding/json field discovery for arbitrary
// embedded fields or custom codecs.
//
// When a previous value is merged, objects are re-encoded with sorted keys, as
// encoding/json sorts map keys; without one the encoding of value is returned
// as is. If previous is not valid JSON of the shape value's type needs, the
// decoder's error is returned.
func Merge(previous json.RawMessage, value any) (json.RawMessage, error) {
	next, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return mergeValue(previous, next, reflect.TypeOf(value))
}

func mergeValue(previous, next json.RawMessage, typ reflect.Type) (json.RawMessage, error) {
	previous, next = bytes.TrimSpace(previous), bytes.TrimSpace(next)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if string(next) == "null" || len(previous) == 0 || string(previous) == "null" {
		return next, nil
	}
	// Custom codecs and RawMessage own their whole value. Time, for example,
	// serializes as a string rather than an object of its Go fields.
	if typ.Implements(reflect.TypeFor[json.Marshaler]()) || typ == reflect.TypeFor[json.RawMessage]() {
		return next, nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		return mergeStruct(previous, next, typ)
	case reflect.Map:
		if typ.Key().Kind() != reflect.String {
			return next, nil
		}
		return mergeMap(previous, next, typ)
	case reflect.Slice:
		return mergeSlice(previous, next, typ)
	default:
		return next, nil
	}
}

// decodeObjects splits two JSON objects into raw fields, previous first.
func decodeObjects(previous, next json.RawMessage) (oldFields, newFields map[string]json.RawMessage, err error) {
	if err := json.Unmarshal(previous, &oldFields); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(next, &newFields); err != nil {
		return nil, nil, err
	}
	return oldFields, newFields, nil
}

// mergeStruct keeps previous fields the type does not own and merges those it does.
func mergeStruct(previous, next json.RawMessage, typ reflect.Type) (json.RawMessage, error) {
	oldFields, newFields, err := decodeObjects(previous, next)
	if err != nil {
		return nil, err
	}
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		prior := oldFields[name]
		for key, raw := range oldFields {
			if strings.EqualFold(key, name) {
				if prior == nil {
					prior = raw
				}
				delete(oldFields, key)
			}
		}
		if raw, exists := newFields[name]; exists {
			merged, err := mergeValue(prior, raw, field.Type)
			if err != nil {
				return nil, err
			}
			newFields[name] = merged
		}
	}
	for name, raw := range newFields {
		oldFields[name] = raw
	}
	return json.Marshal(oldFields)
}

// mergeMap removes entries absent from next; only retained entries carry extensions.
func mergeMap(previous, next json.RawMessage, typ reflect.Type) (json.RawMessage, error) {
	oldFields, newFields, err := decodeObjects(previous, next)
	if err != nil {
		return nil, err
	}
	for key, raw := range newFields {
		merged, err := mergeValue(oldFields[key], raw, typ.Elem())
		if err != nil {
			return nil, err
		}
		newFields[key] = merged
	}
	return json.Marshal(newFields)
}

// mergeSlice matches array items by "id", not position: a sliding window (flood
// arrivals) can remove entries from the front before a later rewrite. Slices
// without struct items that carry an id are replaced whole.
func mergeSlice(previous, next json.RawMessage, typ reflect.Type) (json.RawMessage, error) {
	elem := typ.Elem()
	if elem.Kind() != reflect.Struct {
		return next, nil
	}
	id, exists := elem.FieldByName("ID")
	if !exists || id.Tag.Get("json") != "id" {
		return next, nil
	}
	var oldItems, newItems []json.RawMessage
	if err := json.Unmarshal(previous, &oldItems); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(next, &newItems); err != nil {
		return nil, err
	}
	byID := map[string]json.RawMessage{}
	for _, raw := range oldItems {
		key, err := itemID(raw)
		if err != nil {
			return nil, err
		}
		byID[key] = raw
	}
	for i, raw := range newItems {
		key, err := itemID(raw)
		if err != nil {
			return nil, err
		}
		merged, err := mergeValue(byID[key], raw, elem)
		if err != nil {
			return nil, err
		}
		newItems[i] = merged
	}
	return json.Marshal(newItems)
}

func itemID(raw json.RawMessage) (string, error) {
	var key struct {
		ID string `json:"id"`
	}
	err := json.Unmarshal(raw, &key)
	return key.ID, err
}
