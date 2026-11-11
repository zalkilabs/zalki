package model

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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

// IsCanonicalUUID reports whether id already has the persisted UUID form.
func IsCanonicalUUID(id string) bool {
	canonical, err := UUID(id)
	return err == nil && canonical == id
}

// ValidateSessionAddress checks syntax without consulting an adapter registry.
func ValidateSessionAddress(id string) error {
	parts := strings.Split(id, ":")
	if len(parts) != 2 || !ValidKind(parts[0]) {
		return NewError(CodeInvalidIdentity, "use the complete kind:UUID returned by list_agents")
	}
	uuid, err := UUID(parts[1])
	if err != nil {
		return err
	}
	if uuid != parts[1] {
		return NewError(CodeInvalidIdentity, "invalid session identity")
	}
	return nil
}

// IsPluginInboxAddress recognizes only an exact installation address.
func IsPluginInboxAddress(to string) bool {
	id, ok := strings.CutPrefix(to, "plugin:")
	return ok && IsCanonicalUUID(id)
}

// NewUUID allocates a random UUID for a new persisted identity.
func NewUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6], b[8] = b[6]&15|64, b[8]&63|128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// DeriveUUID uses length-prefixed components within an identity namespace.
func DeriveUUID(namespace string, parts ...string) string {
	h := sha256.New()
	h.Write([]byte(namespace))
	for _, part := range parts {
		_, _ = fmt.Fprintf(h, "\x00%d:%s", len(part), part) // hash.Hash writes never fail
	}
	b := h.Sum(nil)
	b[6], b[8] = b[6]&15|128, b[8]&63|128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:16])
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
