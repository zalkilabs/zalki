package journal_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
)

type flat struct {
	Schema int    `json:"schema"`
	Name   string `json:"name,omitempty"`
	Count  int    // No tag: owned under its Go name.
	secret string //nolint:unused // Unexported fields are never encoded or owned.
}

type mode struct {
	Mode string `json:"mode,omitempty"`
}

type withNested struct {
	Nested *mode `json:"nested,omitempty"`
}

type withMap struct {
	Tags map[string]mode `json:"tags,omitempty"`
}

type item struct {
	ID   string `json:"id"`
	Note string `json:"note,omitempty"`
}

type withItems struct {
	Items []item `json:"items,omitempty"`
}

type namedItem struct {
	Name string `json:"name"`
}

type withPlain struct {
	Plain []namedItem `json:"plain,omitempty"`
}

type withRaw struct {
	Raw json.RawMessage `json:"raw,omitempty"`
}

type withTime struct {
	At time.Time `json:"at"`
}

func TestMerge(t *testing.T) {
	when := time.Date(2026, 11, 6, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		name     string
		previous string
		value    any
		want     string
	}{
		{"no previous returns the value's own encoding", ``, flat{Schema: 1, Name: "a"}, `{"schema":1,"name":"a","Count":0}`},
		{"null previous returns the value's own encoding", `null`, flat{Schema: 1}, `{"schema":1,"Count":0}`},
		{"a typed nil value encodes as null", `{"a":1}`, (*flat)(nil), `null`},
		{"a pointer merges like its struct", `{"future":true}`, &flat{Schema: 1}, `{"Count":0,"future":true,"schema":1}`},

		{"fields the type does not own are preserved", `{"schema":1,"future":{"x":[1]},"name":"old"}`, flat{Schema: 1, Name: "new"},
			`{"Count":0,"future":{"x":[1]},"name":"new","schema":1}`},
		{"a cleared omitempty field is removed, not kept", `{"schema":1,"name":"old","future":1}`, flat{Schema: 1},
			`{"Count":0,"future":1,"schema":1}`},
		{"ownership ignores key capitalization", `{"NAME":"old","Other":1}`, flat{Schema: 1},
			`{"Count":0,"Other":1,"schema":1}`},
		{"a field without a json tag is owned under its Go name", `{"Count":9,"count":8}`, flat{Schema: 1, Count: 3},
			`{"Count":3,"schema":1}`},

		{"a nested struct keeps its unknown fields", `{"nested":{"mode":"a","ext":1}}`, withNested{Nested: &mode{Mode: "b"}},
			`{"nested":{"ext":1,"mode":"b"}}`},
		{"an omitted nested struct is removed", `{"nested":{"mode":"a","ext":1},"future":2}`, withNested{},
			`{"future":2}`},

		{"map entries absent from the value are removed", `{"tags":{"a":{"mode":"x","ext":1},"b":{"mode":"y"}}}`, withMap{Tags: map[string]mode{"a": {Mode: "z"}}},
			`{"tags":{"a":{"ext":1,"mode":"z"}}}`},
		{"a new map entry has nothing to merge", `{"tags":{"a":{"ext":1}}}`, withMap{Tags: map[string]mode{"b": {Mode: "z"}}},
			`{"tags":{"b":{"mode":"z"}}}`},
		{"a top-level map drops absent keys", `{"a":0,"b":2}`, map[string]int{"a": 1}, `{"a":1}`},

		{"slice items merge by id, not position", `{"items":[{"id":"1","note":"n1","ext":"e1"},{"id":"2","ext":"e2"}]}`,
			withItems{Items: []item{{ID: "2", Note: "m"}, {ID: "1"}}},
			`{"items":[{"ext":"e2","id":"2","note":"m"},{"ext":"e1","id":"1"}]}`},
		{"a slice item with an unknown id is taken as is", `{"items":[{"id":"1","ext":1}]}`, withItems{Items: []item{{ID: "9", Note: "n"}}},
			`{"items":[{"id":"9","note":"n"}]}`},
		{"slices without an id field are replaced whole", `{"plain":[{"name":"a","ext":1}]}`, withPlain{Plain: []namedItem{{Name: "a"}}},
			`{"plain":[{"name":"a"}]}`},

		{"RawMessage values are replaced whole", `{"raw":{"a":1}}`, withRaw{Raw: json.RawMessage(`{"b":2}`)}, `{"raw":{"b":2}}`},
		{"values with a custom encoding are replaced whole", `{"at":"2020-01-01T00:00:00Z"}`, withTime{At: when}, `{"at":"2026-11-06T09:00:00Z"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := journal.Merge(json.RawMessage(test.previous), test.value)
			if err != nil || string(got) != test.want {
				t.Fatalf("got %s, %v\nwant %s", got, err, test.want)
			}
		})
	}
}

func TestMergeRejectsShapesTheTypeCannotMerge(t *testing.T) {
	tests := []struct {
		name     string
		previous string
		value    any
	}{
		{"a struct over a JSON array", `[1]`, flat{Schema: 1}},
		{"a struct over truncated JSON", `{"schema":`, flat{Schema: 1}},
		{"a map over a JSON array", `[1]`, map[string]int{"a": 1}},
		{"a slice field over a JSON object", `{"items":{"id":"1"}}`, withItems{Items: []item{{ID: "1"}}}},
		{"a slice item that is not an object", `{"items":[1]}`, withItems{Items: []item{{ID: "1"}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := journal.Merge(json.RawMessage(test.previous), test.value)
			if err == nil {
				t.Fatalf("got %s, want an error", got)
			}
		})
	}
	t.Run("a value that cannot be encoded", func(t *testing.T) {
		if _, err := journal.Merge(nil, make(chan int)); err == nil {
			t.Fatal("want an error")
		}
	})
}

// Merging a record into itself, and merging twice, changes nothing more.
func TestMergeIsIdempotent(t *testing.T) {
	previous := json.RawMessage(`{"schema":1,"future":{"x":1},"name":"old"}`)
	value := flat{Schema: 1, Name: "new"}
	once, err := journal.Merge(previous, value)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := journal.Merge(once, value)
	if err != nil || string(once) != string(twice) {
		t.Fatalf("second merge changed the record: %s -> %s (%v)", once, twice, err)
	}
}
