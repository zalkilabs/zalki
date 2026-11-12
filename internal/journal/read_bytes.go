package journal

import (
	"errors"
	"io"
	"os"
	"syscall"
)

// ReadBytes returns exact file bytes, including surrounding whitespace. It
// rejects symlinks, nonregular files and content over limit, including growth
// after stat. Callers validate parent directories and hold their journal lock.
func ReadBytes(path string, limit int) ([]byte, error) {
	return readBytes(path, limit, nil)
}

// afterStat observes the opened descriptor; nil leaves normal reads unchanged.
func readBytes(path string, limit int, afterStat func(*os.File)) ([]byte, error) {
	if limit < 0 || limit == int(^uint(0)>>1) {
		return nil, ErrInvalidFile
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: caller validates parents; NOFOLLOW rejects final links and NONBLOCK avoids opening a FIFO indefinitely.
	if errors.Is(err, syscall.ELOOP) {
		return nil, ErrInvalidFile
	}
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // Read-only descriptor has no pending writes.
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > int64(limit) {
		return nil, ErrInvalidFile
	}
	if afterStat != nil {
		afterStat(file)
	}
	raw, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, ErrInvalidFile
	}
	return raw, nil
}
