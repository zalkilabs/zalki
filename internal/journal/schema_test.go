package journal_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
)

func TestCheckSchema(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		supported int
		want      int
		wantErr   string // "schema", "json" or empty
	}{
		{"missing field is version 0", `{"id":"a"}`, 2, 0, ""},
		{"current version", `{"schema":2}`, 2, 2, ""},
		{"older version", `{"schema":1,"x":true}`, 2, 1, ""},
		{"newer version", `{"schema":3}`, 2, 0, "schema"},
		{"negative version", `{"schema":-1}`, 2, 0, "schema"},
		{"zero supported rejects any positive version", `{"schema":1}`, 0, 0, "schema"},
		{"not a JSON object", `[1]`, 2, 0, "json"},
		{"truncated JSON", `{"schema":`, 2, 0, "json"},
		{"schema of the wrong type", `{"schema":"2"}`, 2, 0, "json"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := journal.CheckSchema(json.RawMessage(test.raw), test.supported)
			var schema *journal.SchemaError
			switch test.wantErr {
			case "":
				if err != nil || got != test.want {
					t.Fatalf("got %d, %v; want %d", got, err, test.want)
				}
			case "schema":
				if !errors.As(err, &schema) || schema.Supported != test.supported || got != 0 {
					t.Fatalf("got %d, %v; want a SchemaError", got, err)
				}
			default:
				if err == nil || errors.As(err, &schema) {
					t.Fatalf("got %d, %v; want a JSON error", got, err)
				}
			}
		})
	}
	t.Run("the error carries the offending version", func(t *testing.T) {
		_, err := journal.CheckSchema(json.RawMessage(`{"schema":9}`), 2)
		var schema *journal.SchemaError
		if !errors.As(err, &schema) || schema.Version != 9 {
			t.Fatalf("error = %v", err)
		}
	})
}
