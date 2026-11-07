package bus

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type Store struct {
	Dir string
	// Registry is required; NewStore rejects a nil or empty registry.
	Registry *agent.Registry
	Logger   *slog.Logger
}

// NewStore returns a Store that resolves agent kinds through registry.
func NewStore(dir string, registry *agent.Registry, logger *slog.Logger) (Store, error) {
	if len(registry.Kinds()) == 0 {
		return Store{}, errors.New("an agent registry with at least one adapter is required")
	}
	return Store{Dir: dir, Registry: registry, Logger: logger}, nil
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
	if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return errors.New("state directory is owned by a different user")
	}
	return nil
}

func (s Store) locked(fn func() error) error {
	return s.lockedMode(fn, false)
}

// readLocked preserves normal reply recovery when formats are compatible. A
// newer marker disables recovery and permits reads without applying old writes.
func (s Store) readLocked(fn func() error) error {
	return s.lockedMode(fn, true)
}

func (s Store) lockedMode(fn func() error, readOnly bool) error {
	if err := privateDir(s.Dir); err != nil {
		return err
	}
	for _, dir := range []string{"sessions", "messages"} {
		if err := privateDir(filepath.Join(s.Dir, dir)); err != nil {
			return err
		}
	}
	fd, err := journal.OpenLockFile(filepath.Join(s.Dir, "lock"), true)
	if err != nil {
		return err
	}
	defer journal.CloseLockFile(fd) //nolint:errcheck // Closing also releases the journal lock.
	if err := journal.LockExclusive(fd, false); err != nil {
		return err
	}
	defer journal.Unlock(fd) //nolint:errcheck // Descriptor close also releases the lock.
	compatibility := s.CheckWriteCompatibility()
	if compatibility != nil && (!readOnly || eventIntentPresent(s)) {
		return compatibility
	}
	if compatibility == nil {
		if err := recoverEventCommit(s); err != nil {
			return err
		}
		if err := s.recoverReplies(); err != nil {
			s.logEvent(slog.LevelError, "reply.recovery_failed", eventData{Err: err})
			return err
		}
	}
	return fn()
}

// maxStateFileBytes bounds the exact persisted JSON, including its newline.
const maxStateFileBytes = 128 * 1024

func readJSON(path string, target any) error {
	return journal.ReadJSON(path, maxStateFileBytes, target)
}

// encodeStateJSON validates exactly what will reach disk, not the compact
// input. No writer may publish bytes that readJSON would reject by size.
func encodeStateJSON(value any) ([]byte, error) {
	b, err := journal.EncodeJSON(value, maxStateFileBytes)
	if size, ok := err.(*journal.SizeError); ok { //nolint:errorlint // Only the direct limit error is ours; wrapped encoder failures pass through.
		return nil, model.NewError(model.CodeInvalidInput, "encoded state record is %d bytes; limit is %d bytes", size.Size, size.Limit)
	}
	return b, err
}

func writeJSON(path string, value any) error {
	b, err := encodeStateJSON(value)
	if err != nil {
		return err
	}
	return journal.WriteAtomic(path, b)
}
