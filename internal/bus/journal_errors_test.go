package bus

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type failingJournalMarshaler struct{ cause error }

func (m failingJournalMarshaler) MarshalJSON() ([]byte, error) { return nil, m.cause }

func TestJournalEncodingPreservesWrappedEncoderError(t *testing.T) {
	// Only the journal's own limit check is an invalid-input size error. A
	// custom encoder may return that same type; encoding/json wraps it, and
	// the bus must preserve that encoder failure rather than reclassify it.
	cause := &journal.SizeError{Size: 7, Limit: 5}
	_, err := encodeStateJSON(failingJournalMarshaler{cause: cause})
	var encoder *json.MarshalerError
	if !errors.As(err, &encoder) || !errors.Is(err, cause) || model.ErrorCodeOf(err) != "internal" {
		t.Fatalf("encoder failure changed: %T %v (code %q)", err, err, model.ErrorCodeOf(err))
	}
}
