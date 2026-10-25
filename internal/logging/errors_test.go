package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"testing"
)

func TestDecodeErrorKindsDoNotExposePayload(t *testing.T) {
	var target struct{ Status string }
	err := json.Unmarshal([]byte(`{"Status":{"private-field":"private-value"}}`), &target)
	if err == nil {
		t.Fatal("expected a JSON type error")
	}
	for _, test := range []struct {
		err  error
		kind string
	}{
		{fmt.Errorf("private-value: %w", err), "json_type_mismatch"},
		{fmt.Errorf("private-value: %w", io.EOF), "eof"},
		{fmt.Errorf("private-value: %w", io.ErrUnexpectedEOF), "unexpected_eof"},
	} {
		if got := ErrorKind(test.err); got != test.kind {
			t.Fatalf("error category = %q, want %q", got, test.kind)
		}
	}
}
