package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

// mergeRecordJSON preserves fields outside the typed record while replacing
// every field the type owns, including fields omitted because they were cleared.
func mergeRecordJSON(previous json.RawMessage, value any) (json.RawMessage, error) {
	next, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return mergeRecordValue(previous, next, reflect.TypeOf(value))
}

func mergeRecordValue(previous, next json.RawMessage, typ reflect.Type) (json.RawMessage, error) {
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
			merged, err := mergeRecordValue(prior, raw, field.Type)
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
		merged, err := mergeRecordValue(oldFields[key], raw, typ.Elem())
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
		merged, err := mergeRecordValue(byID[key], raw, elem)
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

// recordUpdate carries preservation data only for an exceptional first write
// from a durable reply intent, and obsolete fields consumed by migration.
type recordUpdate struct {
	previous json.RawMessage
	removed  []string
}

// writeRecord is called under the journal lock. Re-read the on-disk record so
// re-registration and crash recovery also preserve fields absent from their
// newly constructed values. A future record is never overwritten by a fresh value.
func (s Store) writeRecord(path string, value any, supported int, updates ...recordUpdate) error {
	update := recordUpdate{}
	if len(updates) != 0 {
		update = updates[0]
	}
	next, err := json.Marshal(value)
	if err != nil {
		return err
	}
	nextSchema, err := checkSchema(next, supported)
	if err != nil {
		return err
	}
	var previous json.RawMessage
	if err := readJSON(path, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(previous) == 0 {
		previous = update.previous
	}
	if len(previous) != 0 {
		if _, err := checkSchema(previous, supported); err != nil {
			return err
		}
		if len(update.removed) != 0 {
			if previous, err = dropFields(previous, update.removed); err != nil {
				return err
			}
		}
	}
	raw, err := mergeRecordJSON(previous, value)
	if err != nil {
		return err
	}
	// Validate before publishing the marker, then write these exact bytes after
	// the marker. Unknown fields are never dropped to make an oversized merge fit.
	encoded, err := encodeStateJSON(raw)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(s.Dir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return fail(CodeInvalidIdentity, "record path is outside state directory")
	}
	family, _, _ := strings.Cut(relative, string(os.PathSeparator))
	version := nextSchema
	if version == 0 {
		version = 1
	}
	if err := s.ensureStateFormat(family, version); err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, encoded)
}

// checkSchema returns the record's schema version and rejects one this build
// does not support.
func checkSchema(raw json.RawMessage, supported int) (int, error) {
	var header struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return 0, err
	}
	if header.Schema < 0 || header.Schema > supported {
		return 0, fail(CodeUnsupportedSchema, "unsupported journal schema %d; upgrade zalki", header.Schema)
	}
	return header.Schema, nil
}

// dropFields removes obsolete fields, matched case-insensitively, from a record.
func dropFields(record json.RawMessage, removed []string) (json.RawMessage, error) {
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(record, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		for _, name := range removed {
			if strings.EqualFold(key, name) {
				delete(fields, key)
				break
			}
		}
	}
	return json.Marshal(fields)
}

// futureRecord is used only to protect cleanup candidates. Corrupt old records
// keep their existing cleanup policy, but a readable future header is kept.
func futureRecord(path string, supported int) bool {
	var header struct {
		Schema int `json:"schema"`
	}
	return readJSON(path, &header) == nil && header.Schema > supported
}
