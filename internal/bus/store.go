package bus

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/platform"
	"github.com/zalkilabs/zalki/internal/statefile"
)

var errJournalRootChanged = model.NewError(model.CodeConflict, "Zalki state changed while waiting for its journal lock; no journal recovery or callback was run. Start a fresh command")

// Private process-test gate; production never pauses between open and flock.
var storeLockOpened = func() {}

type Store struct {
	pluginDelivery pluginDeliveryCheck
	dir            string
	// registry is required; NewStore rejects a nil or empty registry.
	registry *agent.Registry
	logger   *slog.Logger
	// maxBodyBytes limits fresh admission. Zero uses the fixed hard ceiling.
	maxBodyBytes int
	// configFile pins the selected settings file in trusted child guidance.
	configFile string
}

// StoreOption sets one optional part of a Store.
type StoreOption func(*Store)

// WithRegistry sets the registry that resolves agent kinds.
func WithRegistry(registry *agent.Registry) StoreOption {
	return func(s *Store) { s.registry = registry }
}

// WithLogger sets the logger.
func WithLogger(logger *slog.Logger) StoreOption { return func(s *Store) { s.logger = logger } }

// WithMaxBodyBytes sets the limit for fresh message bodies. Zero uses the fixed hard ceiling.
func WithMaxBodyBytes(n int) StoreOption { return func(s *Store) { s.maxBodyBytes = n } }

// WithConfigFile sets the settings file that trusted child guidance names.
func WithConfigFile(path string) StoreOption { return func(s *Store) { s.configFile = path } }

// WithPluginDelivery sets the check that decides whether a plugin may receive a message.
func WithPluginDelivery(check pluginDeliveryCheck) StoreOption {
	return func(s *Store) { s.pluginDelivery = check }
}

// StoreAt returns a Store for the state directory dir. It does not check the
// registry: use NewStore when the Store must resolve agent kinds.
func StoreAt(dir string, opts ...StoreOption) Store {
	return Store{dir: dir}.With(opts...)
}

// With returns a copy of the Store with the options applied.
func (s Store) With(opts ...StoreOption) Store {
	for _, opt := range opts {
		opt(&s)
	}
	return s
}

// Dir returns the state directory.
func (s Store) Dir() string { return s.dir }

// Registry returns the registry that resolves agent kinds.
func (s Store) Registry() *agent.Registry { return s.registry }

// Logger returns the logger. It may be nil.
func (s Store) Logger() *slog.Logger { return s.logger }

// NewStore returns a Store that resolves agent kinds through registry. The
// body limit starts at the model default; options may change it.
func NewStore(dir string, registry *agent.Registry, logger *slog.Logger, opts ...StoreOption) (Store, error) {
	if len(registry.Kinds()) == 0 {
		return Store{}, errors.New("an agent registry with at least one adapter is required")
	}
	return Store{dir: dir, registry: registry, logger: logger, maxBodyBytes: model.MaxMessageBodyBytes}.With(opts...), nil
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
	if err := privateDir(s.dir); err != nil {
		return err
	}
	for _, dir := range []string{"sessions", "messages", openMessagesDir} {
		if err := privateDir(filepath.Join(s.dir, dir)); err != nil {
			return err
		}
	}
	fd, err := platform.OpenLockFile(filepath.Join(s.dir, "lock"), true)
	if err != nil {
		return err
	}
	defer platform.CloseLockFile(fd) //nolint:errcheck // Closing also releases the journal lock.
	storeLockOpened()
	if err := platform.LockExclusive(fd, false); err != nil {
		return err
	}
	defer platform.Unlock(fd) //nolint:errcheck // Descriptor close also releases the lock.
	linked, err := platform.Linked(fd, filepath.Join(s.dir, "lock"))
	if err != nil {
		return err
	}
	if !linked {
		return errJournalRootChanged
	}
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
		err := statefile.ReadJSON(path, maxStateFileBytes, &disk)
		*message = disk.Message
		message.PluginBinding = disk.Binding
		return err
	}
	if runtime, ok := target.(*RuntimeInfo); ok {
		return readRuntimeRecord(path, runtime)
	}
	return statefile.ReadJSON(path, maxStateFileBytes, target)
}

// encodeStateJSON validates exactly what will reach disk, not the compact
// input. No writer may publish bytes that readJSON would reject by size.
func encodeStateJSON(value any) ([]byte, error) {
	switch message := value.(type) {
	case model.Message:
		value = storedPluginMessage{Message: message, Binding: message.PluginBinding}
	case RuntimeInfo:
		value = runtimeRecord{Schema: recordSchema, RuntimeInfo: message}
	case *RuntimeInfo:
		if message != nil {
			value = runtimeRecord{Schema: recordSchema, RuntimeInfo: *message}
		}
	case *model.Message:
		if message != nil {
			value = storedPluginMessage{Message: *message, Binding: message.PluginBinding}
		}
	}
	b, err := statefile.EncodeJSON(value, maxStateFileBytes)
	if size, ok := err.(*statefile.SizeError); ok { //nolint:errorlint // Only the direct limit error is ours; wrapped encoder failures pass through.
		return nil, model.NewError(model.CodeInvalidInput, "encoded state record is %d bytes; limit is %d bytes", size.Size, size.Limit)
	}
	return b, err
}
