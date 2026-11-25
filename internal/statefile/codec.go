package statefile

import (
	"encoding/json"
	"errors"
	"fmt"
)

// ErrInvalidFile reports a record file that is not a regular file or is larger
// than the read limit. A symbolic link is not a regular file.
var ErrInvalidFile = errors.New("invalid state file")

// ErrInvalidData identifies stored data that is well-formed JSON but not a
// valid record, and the input that the strict decoder rejects as a whole. Its
// text is the one stored state has always reported; it is kept so that error
// messages do not change. It is a separate identity from [ErrInvalidFile].
var ErrInvalidData = errors.New("invalid journal batch")

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
// and a final newline. It measures exactly the bytes that [fsutil.WriteAtomic] will
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
