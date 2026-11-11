package bus

import (
	"os"
	"path/filepath"
	"testing"
)

// unreadableFile is one way a message file cannot be read. A nil content
// truncates the existing file to half of its bytes.
type unreadableFile struct {
	name    string
	content []byte
	mode    os.FileMode
}

var (
	truncatedMessage = unreadableFile{"truncated", []byte(`{"schema":1,"id":"`), 0600}
	notJSONMessage   = unreadableFile{"not-json", []byte("not json"), 0600}
	modeZeroMessage  = unreadableFile{"unreadable-mode", []byte(`{}`), 0000}
	halfMessage      = unreadableFile{"half", nil, 0600}
)

// unreadableMessages lists the kinds a scan must skip and count.
func unreadableMessages() []unreadableFile {
	return []unreadableFile{truncatedMessage, notJSONMessage, modeZeroMessage}
}

// writeUnreadableMessage makes messages/<id>.json unreadable in the given way
// and returns the bytes it replaced (nil for a new file). Cleanup restores the
// mode, so the test directory can be removed.
func writeUnreadableMessage(t *testing.T, store Store, id string, kind unreadableFile) []byte {
	t.Helper()
	path := filepath.Join(store.Dir, "messages", id+".json")
	original, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	content := kind.content
	if content == nil {
		content = original[:len(original)/2]
	}
	if err := os.WriteFile(path, content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, kind.mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0600) })
	return original
}

// newUnreadableMessage writes an unreadable file under a new UUID name.
func newUnreadableMessage(t *testing.T, store Store, kind unreadableFile) string {
	t.Helper()
	id := newID()
	writeUnreadableMessage(t, store, id, kind)
	return id
}

// restoreMessage puts back the bytes writeUnreadableMessage returned.
func restoreMessage(t *testing.T, store Store, id string, original []byte) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(store.Dir, "messages", id+".json"), original, 0600); err != nil {
		t.Fatal(err)
	}
}
