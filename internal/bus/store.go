package bus

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

type Store struct {
	Dir      string
	Registry *Registry
	Logger   *slog.Logger
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("state directory must be a private directory, not a symlink: %s", path)
	}
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return errors.New("state directory is owned by a different user")
	}
	return nil
}

func (s Store) locked(fn func() error) error {
	if err := privateDir(s.Dir); err != nil {
		return err
	}
	for _, dir := range []string{"sessions", "messages"} {
		if err := privateDir(filepath.Join(s.Dir, dir)); err != nil {
			return err
		}
	}
	fd, err := syscall.Open(filepath.Join(s.Dir, "lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	if err := s.recoverReplies(); err != nil {
		s.logEvent(slog.LevelError, "reply.recovery_failed", eventData{Err: err})
		return err
	}
	return fn()
}

func readJSON(path string, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 128*1024 {
		return errors.New("invalid state file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, target)
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(b, '\n'))
}
