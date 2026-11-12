package install

import (
	"encoding/json"
	"testing"
)

func TestEqualJSONComparesNumbersByValue(t *testing.T) {
	n := func(s string) json.Number { return json.Number(s) }
	tests := []struct {
		name string
		a, b any
		want bool
	}{
		{"5 and int 5", n("5"), 5, true},
		{"5.0 and 5", n("5.0"), 5, true},
		{"5e0 and float 5", n("5e0"), float64(5), true},
		{"5.50 and 5.5", n("5.50"), n("5.5"), true},
		{"0.10 and 0.1", n("0.10"), 0.1, true},
		{"negative zero", n("-0"), 0, true},
		{"2^53+1 does not collapse", n("9007199254740993"), float64(9007199254740992), false},
		{"2^53+1 equals itself", n("9007199254740993"), n("9007199254740993"), true},
		{"huge exponents", n("1e400"), n("10e399"), true},
		{"different numbers", n("5"), n("6"), false},
		{"number and string", n("5"), "5", false},
		{"string and number", "5", n("5"), false},
		{"bool and string", true, "true", false},
		{"nested numbers", map[string]any{"hooks": []any{map[string]any{"timeout": n("5.0")}}}, map[string]any{"hooks": []any{map[string]any{"timeout": 5}}}, true},
		{"extra key", map[string]any{"a": n("1")}, map[string]any{"a": 1, "b": 2}, false},
		{"other key", map[string]any{"a": n("1")}, map[string]any{"b": 1}, false},
		{"longer list", []any{n("1")}, []any{1, 1}, false},
		{"list order", []any{n("1"), n("2")}, []any{2, 1}, false},
		{"null", nil, nil, true},
	}
	for _, test := range tests {
		if got := equalJSON(test.a, test.b); got != test.want {
			t.Errorf("%s: equalJSON = %v, want %v", test.name, got, test.want)
		}
	}
}
