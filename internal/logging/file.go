package logging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// All processes take the same persistent sidecar lock before rotating or
// appending. Opening the current file per entry avoids stale descriptors after
// another hook/MCP process rotates it. Each JSON entry is one append operation.
type rotatingWriter struct {
	path      string
	stateRoot string
	maxBytes  int64
	backups   int
}

func privateLogDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return errors.New("log directory must be private and owned by this OS user")
	}
	return nil
}

func privateLogFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_WRONLY|syscall.O_APPEND|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err == nil {
		stat := info.Sys().(*syscall.Stat_t)
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 {
			err = errors.New("log file must be a private regular file owned by this OS user, without hard links")
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	if w.stateRoot != "" {
		// Bad arguments and read-only previews must not create a journal just
		// to record diagnostics. Explicit file/stderr sinks have no such guard.
		info, err := os.Lstat(w.stateRoot)
		if errors.Is(err, os.ErrNotExist) {
			return len(p), nil
		} else if err != nil {
			return 0, err
		}
		if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
			return 0, errors.New("state root for default logs must be a private directory owned by this OS user")
		}
	}
	if int64(len(p)) > w.maxBytes {
		return 0, errors.New("log entry exceeds file size limit")
	}
	if err := privateLogDirectory(filepath.Dir(w.path)); err != nil {
		return 0, err
	}
	lock, err := privateLogFile(w.path + ".lock")
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return 0, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	f, err := privateLogFile(w.path)
	if err != nil {
		return 0, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return 0, err
	}
	if info.Size()+int64(len(p)) > w.maxBytes {
		if err := f.Close(); err != nil {
			return 0, err
		}
		if err := w.rotate(); err != nil {
			return 0, err
		}
		f, err = privateLogFile(w.path)
		if err != nil {
			return 0, err
		}
	}
	n, writeErr := f.Write(p)
	closeErr := f.Close()
	return n, errors.Join(writeErr, closeErr)
}

func (w *rotatingWriter) rotate() error {
	// Refuse unsafe backup entries before changing any of them.
	for i := 1; i <= w.backups; i++ {
		path := fmt.Sprintf("%s.%d", w.path, i)
		if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		f, err := privateLogFile(path)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	for i := w.backups; i > 1; i-- {
		if err := os.Rename(fmt.Sprintf("%s.%d", w.path, i-1), fmt.Sprintf("%s.%d", w.path, i)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.Rename(w.path, w.path+".1")
}
