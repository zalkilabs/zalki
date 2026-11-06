package model

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var kindPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// UUID validates an exact session or message UUID and returns its lower-case form.
// It accepts upper-case hex, without trimming or changing persisted identity.
func UUID(s string) (string, error) {
	s = strings.ToLower(s)
	if !uuidPattern.MatchString(s) {
		return "", NewError(CodeInvalidIdentity, "expected an exact session/message UUID")
	}
	return s, nil
}

// NativeID returns a client's explicit session ID, or removes a matching legacy
// kind prefix. It does not validate or alter the session value.
func NativeID(v Session) string {
	if v.NativeID != "" {
		return v.NativeID
	}
	return strings.TrimPrefix(v.ID, v.Kind+":")
}

// ValidKind reports whether kind is a lower-case agent identifier. It performs
// syntax validation only, without requiring an adapter registration.
func ValidKind(kind string) bool { return kindPattern.MatchString(kind) }

// ValidName reports whether a label is nonempty, valid UTF-8, at most 512 bytes,
// already trimmed and free of control characters. It does not rewrite labels.
func ValidName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && len(name) <= 512 && utf8.ValidString(name) &&
		!strings.ContainsFunc(name, unicode.IsControl)
}
