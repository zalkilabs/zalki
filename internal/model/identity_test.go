// These tests use independent literal inputs and expected identity rules.
package model

import (
	"errors"
	"strings"
	"testing"
)

func TestUUID(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"01234567-89ab-cdef-0123-456789abcdef", "01234567-89ab-cdef-0123-456789abcdef"},
		{"ABCDEF01-2345-6789-ABCD-EF0123456789", "abcdef01-2345-6789-abcd-ef0123456789"},
		{"00000000-0000-0000-0000-000000000000", "00000000-0000-0000-0000-000000000000"},
		{"ffffffff-ffff-ffff-ffff-ffffffffffff", "ffffffff-ffff-ffff-ffff-ffffffffffff"},
		{"", ""}, {" 01234567-89ab-cdef-0123-456789abcdef", ""},
		{"01234567-89ab-cdef-0123-456789abcdef\n", ""}, {"0123456789abcdef0123456789abcdef", ""},
		{"01234567-89ab-cdef-0123-456789abcdeg", ""}, {"alpha:01234567-89ab-cdef-0123-456789abcdef", ""},
		{"01234567-89ab-cdef-0123-456789abcde", ""}, {"01234567-89ab-cdef-0123-456789abcdef0", ""},
	} {
		t.Run(test.input, func(t *testing.T) {
			got, err := UUID(test.input)
			if got != test.want {
				t.Fatalf("UUID(%q)=%q, want %q", test.input, got, test.want)
			}
			if test.want != "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Code != "invalid_identity" || err.Error() != "invalid_identity: expected an exact session/message UUID" {
				t.Fatalf("UUID error lost its code, type or exact text: %v", err)
			}
		})
	}
}

func TestValidKind(t *testing.T) {
	for _, test := range []struct {
		kind string
		want bool
	}{
		{"a", true}, {"alpha", true}, {"a0-9", true}, {"a--", true},
		{"", false}, {"Alpha", false}, {"0alpha", false}, {"-alpha", false},
		{"a_b", false}, {"a:b", false}, {"a/b", false}, {"a b", false},
		{"a\n", false}, {"é", false}, {" a", false}, {"a\x00", false},
	} {
		t.Run(test.kind, func(t *testing.T) {
			if got := ValidKind(test.kind); got != test.want {
				t.Fatalf("ValidKind(%q)=%v, want %v", test.kind, got, test.want)
			}
		})
	}
}

func TestValidName(t *testing.T) {
	for _, test := range []struct {
		name, input string
		want        bool
	}{
		{"plain", "Native session", true}, {"unicode", "שלום café", true},
		{"ascii byte limit", strings.Repeat("a", 512), true},
		{"unicode byte limit", strings.Repeat("é", 256), true},
		{"internal unicode space", "a\u2028b", true}, {"zero width", "a\u200bb", true},
		{"empty", "", false}, {"leading space", " name", false}, {"trailing space", "name ", false},
		{"unicode trim", "\u00a0name", false}, {"ascii over limit", strings.Repeat("a", 513), false},
		{"unicode over limit", strings.Repeat("é", 257), false},
		{"invalid UTF-8", "a\xffb", false}, {"NUL", "a\x00b", false}, {"tab", "a\tb", false},
		{"newline", "a\nb", false}, {"DEL", "a\x7fb", false}, {"C1 control", "a\u0085b", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := ValidName(test.input); got != test.want {
				t.Fatalf("ValidName(%q)=%v, want %v", test.input, got, test.want)
			}
		})
	}
}
