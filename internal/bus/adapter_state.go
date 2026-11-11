package bus

import (
	"bytes"
	"encoding/json"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

const maxAdapterState = 4096

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
