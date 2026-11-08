package bus

import (
	"bytes"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionReadRequiresExplicitNativeID(t *testing.T) {
	for _, field := range []string{"missing", "empty", "null"} {
		t.Run(field, func(t *testing.T) {
			store := fixtureStore(t)
			target, _ := register(t, store, "codex")
			rewriteRecord(t, store, target.ID, func(record map[string]any) {
				switch field {
				case "missing":
					delete(record, "native_id")
				case "empty":
					record["native_id"] = ""
				case "null":
					record["native_id"] = nil
				}
			})
			path := store.sessionPathMust(t, target.ID)
			before := readCommitFile(t, path)
			if _, err := store.session(target.ID); model.ErrorCodeOf(err) != model.CodeInvalidIdentity {
				t.Fatal("session read inferred an absent native identity", err)
			}
			if after := readCommitFile(t, path); !bytes.Equal(before, after) {
				t.Fatal("rejection changed the session record")
			}
		})
	}
}

func TestSessionWriteDoesNotInferNativeID(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "codex")
	path := store.sessionPathMust(t, target.ID)
	before := readCommitFile(t, path)
	target.NativeID = ""
	if err := store.saveSession(target); model.ErrorCodeOf(err) != model.CodeInvalidIdentity {
		t.Fatal("session write inferred an absent native identity", err)
	}
	if after := readCommitFile(t, path); !bytes.Equal(before, after) {
		t.Fatal("rejected session write changed the record")
	}
}
