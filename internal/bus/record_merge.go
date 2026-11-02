package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
		var oldFields, newFields map[string]json.RawMessage
		if err := json.Unmarshal(previous, &oldFields); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(next, &newFields); err != nil {
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
	case reflect.Map:
		if typ.Key().Kind() != reflect.String {
			return next, nil
		}
		var oldFields, newFields map[string]json.RawMessage
		if err := json.Unmarshal(previous, &oldFields); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(next, &newFields); err != nil {
			return nil, err
		}
		// Removed map entries stay removed; only retained entries carry extensions.
		for key, raw := range newFields {
			merged, err := mergeRecordValue(oldFields[key], raw, typ.Elem())
			if err != nil {
				return nil, err
			}
			newFields[key] = merged
		}
		return json.Marshal(newFields)
	case reflect.Slice:
		// Flood arrivals are identified by UUID, not array position: a sliding
		// window can remove entries from the front before a later rewrite.
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
			var key struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &key); err != nil {
				return nil, err
			}
			byID[key.ID] = raw
		}
		for i, raw := range newItems {
			var key struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &key); err != nil {
				return nil, err
			}
			merged, err := mergeRecordValue(byID[key.ID], raw, elem)
			if err != nil {
				return nil, err
			}
			newItems[i] = merged
		}
		return json.Marshal(newItems)
	default:
		return next, nil
	}
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
	var nextHeader struct {
		Schema int `json:"schema"`
	}
	if err := json.Unmarshal(next, &nextHeader); err != nil {
		return err
	}
	if nextHeader.Schema < 0 || nextHeader.Schema > supported {
		return fail(CodeUnsupportedSchema, "unsupported journal schema %d; upgrade zalki", nextHeader.Schema)
	}
	var previous json.RawMessage
	if err := readJSON(path, &previous); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(previous) == 0 {
		previous = update.previous
	}
	if len(previous) != 0 {
		var header struct {
			Schema int `json:"schema"`
		}
		if err := json.Unmarshal(previous, &header); err != nil {
			return err
		}
		if header.Schema < 0 || header.Schema > supported {
			return fail(CodeUnsupportedSchema, "unsupported journal schema %d; upgrade zalki", header.Schema)
		}
	}
	if len(previous) != 0 && len(update.removed) != 0 {
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(previous, &fields); err != nil {
			return err
		}
		for key := range fields {
			for _, name := range update.removed {
				if strings.EqualFold(key, name) {
					delete(fields, key)
					break
				}
			}
		}
		previous, err = json.Marshal(fields)
		if err != nil {
			return err
		}
	}
	raw, err := mergeRecordJSON(previous, value)
	if err != nil {
		return err
	}
	// Keep the existing journal read bound; this does not change message limits.
	if len(raw) > 128*1024 {
		return fail(CodeInvalidInput, "state record exceeds journal size limit")
	}
	relative, err := filepath.Rel(s.Dir, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return fail(CodeInvalidIdentity, "record path is outside state directory")
	}
	family, _, _ := strings.Cut(relative, string(os.PathSeparator))
	version := nextHeader.Schema
	if version == 0 {
		version = 1
	}
	if err := s.ensureStateFormat(family, version); err != nil {
		return err
	}
	return writeJSON(path, raw)
}

// futureRecord is used only to protect cleanup candidates. Corrupt old records
// keep their existing cleanup policy, but a readable future header is kept.
func futureRecord(path string, supported int) bool {
	var header struct {
		Schema int `json:"schema"`
	}
	return readJSON(path, &header) == nil && header.Schema > supported
}
