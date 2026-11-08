package bus

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

const (
	maxAdapterState = 4096
	maxExtensions   = 32
)

// sessionFields lists the JSON names of every core session field.
var sessionFields = func() []string {
	var names []string
	t := reflect.TypeOf(model.Session{})
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			names = append(names, name)
		}
	}
	return names
}()

// coreField reports whether encoding/json would decode key into a core field.
// Its key matching ignores case, so every case variant belongs to core too.
func coreField(key string) bool {
	for _, name := range sessionFields {
		if strings.EqualFold(key, name) {
			return true
		}
	}
	return false
}

// stateExtensions copies the record's top-level fields that core does not own.
func stateExtensions(record json.RawMessage) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(record, &fields); err != nil {
		return nil, err
	}
	extensions := map[string]json.RawMessage{}
	for key, value := range fields {
		if coreField(key) {
			continue
		}
		if len(extensions) == maxExtensions || len(value) > maxAdapterState {
			return nil, model.NewError(model.CodeInvalidInput, "session record has more than %d extension fields or one over %d bytes", maxExtensions, maxAdapterState)
		}
		extensions[key] = append(json.RawMessage(nil), value...)
	}
	return extensions, nil
}

// decodeAdapterState runs the adapter's codec, if it has one, and returns a
// bounded copy of its result. Adapters without a codec keep state unchanged.
func (s Store) decodeAdapterState(kind string, in agent.StateInput) (json.RawMessage, error) {
	adapter, err := s.registry().Lookup(kind)
	if err != nil {
		return nil, err
	}
	codec, ok := adapter.(agent.SessionStateAdapter)
	if !ok {
		return in.State, nil
	}
	in.State = append(json.RawMessage(nil), in.State...)
	out, err := codec.DecodeState(in)
	if err != nil || out == nil {
		return nil, err
	}
	return boundedAdapterState(out)
}

// boundedAdapterState returns a compacted copy of valid, size-limited state.
func boundedAdapterState(state json.RawMessage) (json.RawMessage, error) {
	if len(state) > maxAdapterState || !json.Valid(state) {
		return nil, model.NewError(model.CodeInvalidInput, "adapter state must be valid JSON of at most %d bytes", maxAdapterState)
	}
	compact := bytes.Buffer{}
	if err := json.Compact(&compact, state); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
