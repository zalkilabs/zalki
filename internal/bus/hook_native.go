package bus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
)

// keepHookBytes starts keeping the hook bytes core reads, only for an adapter
// that declares native fields or reads its event name under a key other than
// the one HookInput decodes itself. The returned function gives the bytes read so
// far, or nil when nothing is kept. Other adapters pay nothing. Nothing is kept
// with the bounded streaming selector, which never keeps the whole payload: it
// picks the event key from its own kept metadata, and native fields are not
// supported with it.
func keepHookBytes(input io.Reader, d agent.AdapterDescriptor, extendedEvent string) (io.Reader, func() []byte) {
	if (len(d.Native) == 0 && decodesEventKeyItself(d.HookEventKey)) || extendedEvent != "" {
		return input, func() []byte { return nil }
	}
	var seen bytes.Buffer
	return io.TeeReader(input, &seen), seen.Bytes
}

// pickNative returns the native fields in the kept bytes, nil when none are kept.
func pickNative(kept []byte, d agent.AdapterDescriptor) (json.RawMessage, error) {
	if len(d.Native) == 0 || kept == nil {
		return nil, nil
	}
	return selectNative(kept, d.Native)
}

// structEventKey is the JSON name of agent.HookInput.Event. HookInput decodes
// that key by itself, so an adapter that declares it needs no second pass over
// the bytes. This is a fast path, not a default: every adapter declares its key.
const structEventKey = "hook_event_name"

// decodesEventKeyItself reports whether the typed decode already reads the
// declared key. It matches ignoring case, as encoding/json does.
func decodesEventKeyItself(key string) bool { return strings.EqualFold(key, structEventKey) }

// useEventKey makes the event name come from the declared key and nothing else.
// For a key HookInput does not decode itself it takes the name out of raw (the
// kept bytes), replacing whatever the typed decode read from hook_event_name.
// It runs after a failed decode too, so that a log can still name the event. It
// returns the decode error when there is one, else the error of the pick.
func useEventKey(event *agent.HookInput, raw []byte, key string, decodeErr error) error {
	if decodesEventKeyItself(key) {
		return decodeErr
	}
	name, err := selectEventName(raw, key)
	event.Event = name
	if decodeErr != nil {
		return decodeErr
	}
	return err
}

// selectNative returns a JSON object with the declared top-level keys of the
// first value in raw, or nil when none is present. A null value counts as
// absent, as it does for the common fields. A value of the wrong type is an
// error, as it is for the common fields.
func selectNative(raw []byte, fields []agent.NativeField) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, err
	}
	kept := map[string]json.RawMessage{}
	var value json.RawMessage
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		field, ok := declaredField(fields, key.(string))
		if !ok || string(value) == "null" {
			continue
		}
		if err := checkNativeKind(field.Kind, value); err != nil {
			return nil, fmt.Errorf("hook field %s has the wrong type", field.Key)
		}
		kept[field.Key] = append(json.RawMessage(nil), value...)
	}
	if len(kept) == 0 {
		return nil, nil
	}
	return json.Marshal(kept)
}

// declaredField matches a payload key to a declared field the way encoding/json
// matches the common fields: ignoring case.
func declaredField(fields []agent.NativeField, key string) (agent.NativeField, bool) {
	for _, field := range fields {
		if strings.EqualFold(field.Key, key) {
			return field, true
		}
	}
	return agent.NativeField{}, false
}

func checkNativeKind(kind agent.NativeKind, value json.RawMessage) error {
	switch kind {
	case agent.NativeString:
		var v string
		return json.Unmarshal(value, &v)
	case agent.NativeStrings:
		var v []string
		return json.Unmarshal(value, &v)
	case agent.NativeInt:
		var v int
		return json.Unmarshal(value, &v)
	}
	return fmt.Errorf("unsupported native field kind %d", kind)
}

// selectEventName returns the string under key in the first value of raw. The key
// matches ignoring case, as the common fields do; a null value is absent and a
// later duplicate wins, as for the common fields. A value of the wrong type is an
// error. Whatever was found before an error is still returned.
func selectEventName(raw []byte, key string) (string, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return "", err
	}
	var name string
	var value json.RawMessage
	for decoder.More() {
		field, err := decoder.Token()
		if err != nil {
			return name, err
		}
		if err := decoder.Decode(&value); err != nil {
			return name, err
		}
		if !strings.EqualFold(field.(string), key) || string(value) == "null" {
			continue
		}
		if err := json.Unmarshal(value, &name); err != nil {
			return "", fmt.Errorf("hook field %s has the wrong type", key)
		}
	}
	return name, nil
}
