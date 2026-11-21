package bus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
)

// nativeCapture starts keeping the hook bytes core reads, only for an adapter
// that declares native fields. The returned function, called after a
// successful decode, picks the declared keys out of them. Other adapters
// pay nothing. Native fields are not supported with the bounded streaming
// selector, which never keeps the whole payload.
func nativeCapture(input io.Reader, d agent.AdapterDescriptor, extendedEvent string) (io.Reader, func() (json.RawMessage, error)) {
	if len(d.Native) == 0 || extendedEvent != "" {
		return input, func() (json.RawMessage, error) { return nil, nil }
	}
	var seen bytes.Buffer
	return io.TeeReader(input, &seen), func() (json.RawMessage, error) { return selectNative(seen.Bytes(), d.Native) }
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
