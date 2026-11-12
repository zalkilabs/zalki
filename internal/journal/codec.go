package journal

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

// ErrInvalidFile reports a record file that is not a regular file or is larger
// than the read limit. A symbolic link is not a regular file.
var ErrInvalidFile = errors.New("invalid state file")

// SizeError reports that an encoded record is larger than the limit. Size and
// Limit count bytes of the exact encoding, including its final newline.
type SizeError struct {
	Size, Limit int
}

func (e *SizeError) Error() string {
	return fmt.Sprintf("encoded record is %d bytes; limit is %d bytes", e.Size, e.Limit)
}

// ReadJSON decodes the JSON file at path into target. The file must be a
// regular file, not a symbolic link, of at most limit bytes; otherwise the
// result is [ErrInvalidFile]. A missing file reports an error for which
// errors.Is(err, os.ErrNotExist) is true. Other failures are returned as the
// standard library reported them.
func ReadJSON(path string, limit int, target any) error {
	b, err := ReadBytes(path, limit)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}

// EncodeJSON returns the record encoding of value: two-space indentation
// and a final newline. It measures exactly the bytes that [WriteAtomic] will
// publish, so a writer can reject an oversized record before it changes
// anything. When the encoding is longer than limit bytes the error is a
// *[SizeError].
func EncodeJSON(value any, limit int) ([]byte, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	b = append(b, '\n')
	if len(b) > limit {
		return nil, &SizeError{Size: len(b), Limit: limit}
	}
	return b, nil
}

// WriteAtomic replaces the file at path with data. The bytes go to a synced
// temporary file in the same directory, which is renamed over path, and the
// directory is synced afterwards. A reader sees the old file or the new one,
// never a partial write. Temporary-file cleanup is attempted on all paths. An
// error from directory sync can occur after replacement, so an error does not prove
// that the old file remains in place.
func WriteAtomic(path string, data []byte) error {
	return fsutil.WriteAtomic(path, data)
}

// SyncDir syncs the directory at path and returns the filesystem error unchanged.
// The caller decides which publication or removal requires this durability step.
func SyncDir(path string) error {
	return fsutil.SyncDir(path)
}
