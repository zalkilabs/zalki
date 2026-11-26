package wal

import (
	"errors"
	"os"
	"testing"

	"github.com/zalkilabs/zalki/internal/statefile"
)

// A redo intent that is valid JSON of the wrong shape is invalid-data with a
// fixed text. Broken JSON never reaches that check: the file read returns the
// decoder's own error, which carries no sentinel. Both are pinned as they are.
func TestDamagedIntentIdentity(t *testing.T) {
	for _, test := range []struct {
		name, intent string
		batch        bool
	}{
		{"unknown field", `{"id":"x","puts":[],"extra":1}`, true},
		{"wrong type", `[]`, true},
		{"not JSON", `{"id":`, false},
		{"empty file", ``, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newBatchFixture(t)
			if err := os.WriteFile(f.log.intentPath(), []byte(test.intent), 0600); err != nil {
				t.Fatal(err)
			}
			err := f.log.Recover(nil)
			if err == nil || errors.Is(err, statefile.ErrInvalidFile) || errors.Is(err, statefile.ErrInvalidData) != test.batch {
				t.Fatalf("Recover = %v, want ErrInvalidData: %v", err, test.batch)
			}
			if test.batch && err.Error() != "invalid journal batch: malformed intent" {
				t.Fatalf("Recover text = %q", err)
			}
		})
	}
}
