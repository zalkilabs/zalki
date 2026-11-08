package bus

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func (s PluginFeed) checkName() error {
	if len(s.name) > 32 || !model.ValidKind(s.name) {
		return model.NewError(model.CodeInvalidInput, "plugin name must match [a-z][a-z0-9-]{0,31}")
	}
	return nil
}

func (s PluginFeed) locked(fn func() error) error {
	if err := s.checkName(); err != nil {
		return err
	}
	return s.store.locked(fn)
}

func (s PluginFeed) pluginDir() string      { return filepath.Join(s.store.Dir, "plugins") }
func (s PluginFeed) pluginDataPath() string { return filepath.Join(s.pluginDir(), s.name+".data") }

func pluginDirectory(path string, create bool) error {
	if create {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || (create && info.Mode().Perm() != 0700) || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t.
		return errors.New("plugin directory must be private and owned by this user")
	}
	return nil
}

// AcquirePluginLease excludes add, remove, other hosts and any still-live child.
// The lease file persists across removal; never unlink or explicitly unlock it.
func (s PluginFeed) AcquirePluginLease() (*os.File, error) {
	if err := s.checkName(); err != nil {
		return nil, err
	}
	if err := pluginDirectory(s.pluginDir(), false); err != nil {
		return nil, err
	}
	return journal.OpenPluginLease(filepath.Join(s.pluginDir(), s.name+".lock"))
}

// AddPlugin creates a disabled installation and its persistent private data dir.
// An existing record is never replaced and abandoned data is never adopted.
func (s PluginFeed) AddPlugin() error { return s.addPlugin(nil) }

// AddExternalPlugin pins an owner-supplied launch specification before installation.
func (s PluginFeed) AddExternalPlugin(executable string, args []string) error {
	if err := s.checkName(); err != nil {
		return err
	}
	if s.name == "fake-notifier" {
		return model.NewError(model.CodeInvalidInput, "fake-notifier is reserved for the built-in plugin")
	}
	launch := &PluginLaunch{Executable: executable, Args: args}
	if err := launch.pin(); err != nil {
		return err
	}
	return s.addPlugin(launch)
}

func (s PluginFeed) addPlugin(launch *PluginLaunch) error {
	if err := s.locked(func() error {
		if err := pluginDirectory(s.pluginDir(), true); err != nil {
			return err
		}
		return errors.Join(journal.SyncDir(filepath.Dir(s.store.Dir)), journal.SyncDir(s.store.Dir))
	}); err != nil {
		return err
	}
	lease, err := s.AcquirePluginLease()
	if err != nil {
		return err
	}
	defer lease.Close() //nolint:errcheck // Closing releases the installation lease.
	return s.locked(func() error {
		p, _, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.ID != "" {
			return model.NewError(model.CodeConflict, "plugin already exists")
		}
		if err := pluginDirectory(s.pluginDataPath(), true); err != nil {
			return err
		}
		data, err := os.Open(s.pluginDataPath())
		if err != nil {
			return err
		}
		entries, err := data.Readdirnames(1)
		closeErr := data.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if len(entries) != 0 {
			return model.NewError(model.CodeConflict, "remove abandoned plugin data before adding")
		}
		_, err = s.savePlugin(pluginRecord{ID: newID(), Name: s.name, Launch: launch})
		return err
	})
}

// RemovePlugin deletes a disabled, stopped installation and its private data.
// Data cleanup runs outside the journal lock, while retaining the name lease.
func (s PluginFeed) RemovePlugin() error {
	lease, err := s.AcquirePluginLease()
	if err != nil {
		return err
	}
	defer lease.Close() //nolint:errcheck // Closing releases the installation lease.
	if err := s.locked(func() error {
		p, _, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.Enabled {
			return model.NewError(model.CodeConflict, "disable the plugin before removing it")
		}
		if err := os.Remove(s.pluginPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return journal.SyncDir(s.pluginDir())
	}); err != nil {
		return err
	}
	if err := os.RemoveAll(s.pluginDataPath()); err != nil {
		return err
	}
	return journal.SyncDir(s.pluginDir())
}
