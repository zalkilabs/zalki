package bus

import (
	"crypto/rand"
	"encoding/hex"

	"github.com/zalkilabs/zalki/internal/model"
)

// UUID forwards canonical identity validation to model.
func UUID(s string) (string, error) { return model.UUID(s) }

func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6], b[8] = b[6]&15|64, b[8]&63|128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}
