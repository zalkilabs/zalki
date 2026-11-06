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

func TestDropFields(t *testing.T) {
	tests := []struct {
		name    string
		record  string
		names   []string
		want    string
		wantErr bool
	}{
		{"removes the named field", `{"a":1,"b":2}`, []string{"a"}, `{"b":2}`, false},
		{"matches case-insensitively", `{"Legacy":1,"b":2}`, []string{"legacy"}, `{"b":2}`, false},
		{"removes every capitalization", `{"legacy":1,"LEGACY":2,"b":3}`, []string{"Legacy"}, `{"b":3}`, false},
		{"ignores absent names", `{"a":1}`, []string{"zzz"}, `{"a":1}`, false},
		{"no names changes nothing", `{"a":1}`, nil, `{"a":1}`, false},
		{"removing everything leaves an empty object", `{"a":1}`, []string{"a"}, `{}`, false},
		{"output keys are sorted", `{"b":1,"a":2}`, nil, `{"a":2,"b":1}`, false},
		{"nested values are kept verbatim", `{"a":{"x":[1,2]},"b":1}`, []string{"b"}, `{"a":{"x":[1,2]}}`, false},
		{"not an object", `[1]`, []string{"a"}, ``, true},
		{"truncated", `{"a":`, []string{"a"}, ``, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := journal.DropFields(json.RawMessage(test.record), test.names)
			if test.wantErr {
				if err == nil {
					t.Fatalf("got %s, want an error", got)
				}
				return
			}
			if err != nil || string(got) != test.want {
				t.Fatalf("got %s, %v; want %s", got, err, test.want)
			}
		})
	}
}
