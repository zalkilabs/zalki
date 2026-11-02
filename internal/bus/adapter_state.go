package bus

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// StateInput is what an adapter's state codec may read. It never carries the
// session: State is a copy of the adapter's own persisted state, and
// Extensions holds a copy of each top-level record field core does not own,
// such as a client's fields from before adapter state existed. Credentials,
// identity and every other core field are removed before the codec runs.
type StateInput struct {
	Extensions map[string]json.RawMessage
	State      json.RawMessage
}

// SessionStateAdapter validates or migrates an adapter's persisted state. It
// returns the normalized current-schema state; core bounds, copies and stores
// only that result. An error leaves the record unread or unchanged.
type SessionStateAdapter interface {
	DecodeState(StateInput) (json.RawMessage, error)
}

// SessionStateMigrationAdapter identifies obsolete top-level extension fields
// consumed by an adapter's migration. Core-owned fields can never be removed.
type SessionStateMigrationAdapter interface {
	LegacyStateFields() []string
}

const (
	maxAdapterState = 4096
	maxExtensions   = 32
)

// sessionFields lists the JSON names of every core session field.
var sessionFields = func() []string {
	var names []string
	t := reflect.TypeOf(Session{})
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
			return nil, fail(CodeInvalidInput, "session record has more than %d extension fields or one over %d bytes", maxExtensions, maxAdapterState)
		}
		extensions[key] = append(json.RawMessage(nil), value...)
	}
	return extensions, nil
}

// decodeAdapterState runs the adapter's codec, if it has one, and returns a
// bounded copy of its result. Adapters without a codec keep state unchanged.
func (s Store) decodeAdapterState(kind string, in StateInput) (json.RawMessage, error) {
	adapter, err := s.registry().Lookup(kind)
	if err != nil {
		return nil, err
	}
	codec, ok := adapter.(SessionStateAdapter)
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
		return nil, fail(CodeInvalidInput, "adapter state must be valid JSON of at most %d bytes", maxAdapterState)
	}
	compact := bytes.Buffer{}
	if err := json.Compact(&compact, state); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
