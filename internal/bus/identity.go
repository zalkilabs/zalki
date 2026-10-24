package bus

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func UUID(s string) (string, error) {
	s = strings.ToLower(s)
	if !uuidPattern.MatchString(s) {
		return "", fail(CodeInvalidIdentity, "expected an exact session/message UUID")
	}
	return s, nil
}

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6], b[8] = b[6]&15|64, b[8]&63|128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// Fixed-width fractional seconds make journal ordering lexicographically stable.
func timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z") }
