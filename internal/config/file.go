package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"
)

const maxConfigBytes = 64 * 1024

func readConfig(path string) ([]byte, bool, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: caller-selected config path, checked through the opened descriptor.
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if errors.Is(err, syscall.ELOOP) {
		return nil, false, fmt.Errorf("%s is a symbolic link. Choose a regular config file owned by the invoking user", path)
	}
	if err != nil {
		return nil, false, fmt.Errorf("%s could not be opened: %w. Check that this config file exists and is readable by the invoking user", path, err)
	}
	defer file.Close() //nolint:errcheck // Read-only descriptor has no pending writes.
	info, err := file.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("%s could not be inspected: %w. Check that this config file exists and is readable by the invoking user", path, err)
	}
	if err := checkPrivateFile(info, uint32(os.Getuid())); err != nil { //nolint:gosec // G115: Unix uid_t fits uint32.
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	raw, err := readBounded(file)
	if err != nil {
		return nil, false, fmt.Errorf("%s: %w", path, err)
	}
	return raw, true, nil
}

func checkPrivateFile(info fs.FileInfo, uid uint32) error {
	if !info.Mode().IsRegular() {
		return errors.New("config is not a regular file. Choose a regular config file owned by the invoking user")
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != uid {
		return errors.New("config belongs to another user. Choose a config file owned by the invoking user")
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("config permissions allow group or other access. Run chmod 600 on this config file")
	}
	if info.Size() > maxConfigBytes {
		return errors.New("config exceeds 65536 bytes. Reduce the config file to at most 65536 bytes")
	}
	return nil
}

func readBounded(reader io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, maxConfigBytes+1))
	if err != nil {
		return nil, fmt.Errorf("config could not be read: %w. Check that this config file exists and is readable by the invoking user", err)
	}
	if len(raw) > maxConfigBytes {
		return nil, errors.New("config exceeds 65536 bytes. Reduce the config file to at most 65536 bytes")
	}
	return raw, nil
}
