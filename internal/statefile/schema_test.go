package statefile_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zalkilabs/zalki/internal/statefile"
)

func TestCheckSchema(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		supported int
		want      int
		wantErr   string // "schema", "json" or empty
	}{
		{"missing field is refused", `{"id":"a"}`, 2, 0, "schema"},
		{"zero version", `{"schema":0}`, 2, 0, "schema"},
		{"null schema", `{"schema":null}`, 2, 0, "schema"},
		{"null record", `null`, 2, 0, "schema"},
		{"current version", `{"schema":2}`, 2, 2, ""},
		{"older version", `{"schema":1,"x":true}`, 2, 0, "schema"},
		{"newer version", `{"schema":3}`, 2, 0, "schema"},
		{"negative version", `{"schema":-1}`, 2, 0, "schema"},
		{"zero supported rejects any positive version", `{"schema":1}`, 0, 0, "schema"},
		{"not a JSON object", `[1]`, 2, 0, "json"},
		{"truncated JSON", `{"schema":`, 2, 0, "json"},
		{"schema of the wrong type", `{"schema":"2"}`, 2, 0, "json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := statefile.CheckSchema(json.RawMessage(test.raw), test.supported)
			verifyCheckSchema(t, got, err, test.supported, test.want, test.wantErr)
		})
	}
	t.Run("the error carries the offending version", func(t *testing.T) {
		_, err := statefile.CheckSchema(json.RawMessage(`{"schema":9}`), 2)
		var schema *statefile.SchemaError
		if !errors.As(err, &schema) || schema.Version != 9 {
			t.Fatalf("error = %v", err)
		}
	})
}

// verifyCheckSchema checks one CheckSchema result: wantErr is "" (the current
// version), "schema" (a SchemaError) or "json" (the decoder's error).
func verifyCheckSchema(t *testing.T, got int, err error, supported, want int, wantErr string) {
	t.Helper()
	var schema *statefile.SchemaError
	switch wantErr {
	case "":
		if err != nil || got != want {
			t.Fatalf("got %d, %v; want %d", got, err, want)
		}
	case "schema":
		if !errors.As(err, &schema) || schema.Supported != supported || got != 0 {
			t.Fatalf("noncurrent schema was not refused: got %d, %v; want a SchemaError", got, err)
		}
	default:
		if err == nil || errors.As(err, &schema) {
			t.Fatalf("got %d, %v; want a JSON error", got, err)
		}
	}
}
