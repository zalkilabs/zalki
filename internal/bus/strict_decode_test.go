package bus_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
)

type strictTarget struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// DecodeStrictRecordJSON is the shared strict reader for state records. Its
// answers are part of the contract: input that is not exactly one valid JSON
// value is the invalid-batch sentinel itself (callers compare with errors.Is),
// and a decoder refusal (unknown field, wrong type) is the decoder's own
// error without that sentinel.
func TestDecodeStrictRecordJSON(t *testing.T) {
	for _, test := range []struct {
		name    string
		raw     string
		want    strictTarget
		invalid bool   // the invalid-batch sentinel, returned as is
		decoder string // "", "unknown", "type"
	}{
		{name: "one object", raw: `{"name":"a","count":2}`, want: strictTarget{Name: "a", Count: 2}},
		{name: "surrounding whitespace", raw: " \n{\"name\":\"a\"}\n\n", want: strictTarget{Name: "a"}},
		{name: "empty input", raw: ``, invalid: true},
		{name: "only whitespace", raw: " \n", invalid: true},
		{name: "truncated", raw: `{"name":`, invalid: true},
		{name: "two values", raw: `{"name":"a"} {"name":"b"}`, invalid: true},
		{name: "trailing garbage", raw: `{"name":"a"} x`, invalid: true},
		{name: "byte order mark", raw: "\xef\xbb\xbf{\"name\":\"a\"}", invalid: true},
		{name: "unknown field", raw: `{"name":"a","other":1}`, decoder: "unknown"},
		{name: "wrong type", raw: `{"name":5}`, decoder: "type"},
		{name: "duplicate field keeps the last value", raw: `{"name":"a","name":"b"}`, want: strictTarget{Name: "b"}},
		{name: "null leaves the target empty", raw: `null`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got strictTarget
			err := bus.DecodeStrictRecordJSON([]byte(test.raw), &got)
			switch {
			case test.invalid:
				if err != journal.ErrInvalidBatch { //nolint:errorlint // The sentinel itself is returned, not a wrapped copy.
					t.Fatalf("error = %v, want the invalid-batch sentinel itself", err)
				}
			case test.decoder != "":
				requireDecoderRefusal(t, err, test.decoder)
			case err != nil || got != test.want:
				t.Fatalf("got %+v, %v; want %+v", got, err, test.want)
			}
		})
	}
}

func requireDecoderRefusal(t *testing.T, err error, kind string) {
	t.Helper()
	if err == nil || errors.Is(err, journal.ErrInvalidBatch) || errors.Is(err, journal.ErrInvalidFile) {
		t.Fatalf("error = %v, want a plain decoder error", err)
	}
	var typeErr *json.UnmarshalTypeError
	switch {
	case kind == "type" && !errors.As(err, &typeErr):
		t.Fatalf("error = %v (%T), want an UnmarshalTypeError", err, err)
	case kind == "unknown" && !strings.Contains(err.Error(), "unknown field"):
		t.Fatalf("error = %v, want an unknown-field error", err)
	}
}
