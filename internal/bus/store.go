package bus

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type Store struct {
	PluginDelivery pluginDeliveryCheck
	Dir            string
	// Registry is required; NewStore rejects a nil or empty registry.
	Registry *agent.Registry
	Logger   *slog.Logger
	// MaxBodyBytes limits fresh admission. Zero uses the fixed hard ceiling.
	MaxBodyBytes int
	// ConfigFile pins the selected settings file in trusted child guidance.
	ConfigFile string
}

// NewStore returns a Store that resolves agent kinds through registry.
func NewStore(dir string, registry *agent.Registry, logger *slog.Logger) (Store, error) {
	if len(registry.Kinds()) == 0 {
		return Store{}, errors.New("an agent registry with at least one adapter is required")
	}
	return Store{Dir: dir, Registry: registry, Logger: logger, MaxBodyBytes: model.MaxMessageBodyBytes}, nil
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return fsutil.CheckPrivateDir(path)
}

// readLocked takes the journal lock for a read. It is the same as locked: it
// also runs recovery, so a read never sees a half-applied event commit or reply.
func (s Store) readLocked(fn func() error) error {
	return s.locked(fn)
}

func (s Store) locked(fn func() error) error {
	if err := privateDir(s.Dir); err != nil {
		return err
	}
	for _, dir := range []string{"sessions", "messages", openMessagesDir} {
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
	if err := recoverEventCommit(s); err != nil {
		return err
	}
	if err := s.recoverReplies(); err != nil {
		s.logEvent(slog.LevelError, "reply.recovery_failed", eventData{Err: err})
		return err
	}
	return fn()
}

// maxStateFileBytes bounds the exact persisted JSON, including its newline.
const maxStateFileBytes = 128 * 1024

func readJSON(path string, target any) error {
	if message, ok := target.(*model.Message); ok {
		var disk storedPluginMessage
		err := journal.ReadJSON(path, maxStateFileBytes, &disk)
		*message = disk.Message
		message.PluginBinding = disk.Binding
		return err
	}
	return journal.ReadJSON(path, maxStateFileBytes, target)
}

// encodeStateJSON validates exactly what will reach disk, not the compact
// input. No writer may publish bytes that readJSON would reject by size.
func encodeStateJSON(value any) ([]byte, error) {
	switch message := value.(type) {
	case model.Message:
		value = storedPluginMessage{Message: message, Binding: message.PluginBinding}
	case *model.Message:
		if message != nil {
			value = storedPluginMessage{Message: *message, Binding: message.PluginBinding}
		}
	}
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
