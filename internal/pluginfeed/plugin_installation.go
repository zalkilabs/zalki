package pluginfeed

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func (s pluginFiles) checkName() error {
	if len(s.name) > 32 || !model.ValidKind(s.name) {
		return model.NewError(model.CodeInvalidInput, "plugin name must match [a-z][a-z0-9-]{0,31}")
	}
	return nil
}

func (s PluginFeed) locked(fn func() error) error {
	return s.write(func(bus.JournalView) error { return fn() })
}

func (s PluginFeed) write(fn func(bus.JournalView) error) error {
	if err := s.checkName(); err != nil {
		return err
	}
	return bus.WithJournalWrite(s.store, fn)
}

func (s pluginFiles) pluginDir() string      { return filepath.Join(s.root, "plugins") }
func (s pluginFiles) pluginDataPath() string { return filepath.Join(s.pluginDir(), s.name+".data") }

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
	return OpenPluginLease(filepath.Join(s.pluginDir(), s.name+".lock"))
}

// AddExternalPlugin pins an owner-supplied launch specification and creates
// a disabled installation with its persistent private data dir. An existing
// record is never replaced and abandoned data is never adopted.
func (s PluginFeed) AddExternalPlugin(executable string, args []string) error {
	if err := s.checkName(); err != nil {
		return err
	}
	launch, err := NewPluginLaunch(executable, args)
	if err != nil {
		return err
	}
	return s.addPlugin(&launch)
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
		if err := s.checkEmptyPluginData(); err != nil {
			return err
		}
		return createPluginInstallation(s, launch)
	})
}

// RemovePlugin disables and removes a stopped installation and its private
// data in one command. Holding the name lease proves no child runs; a held
// lease refuses with nothing removed. Data cleanup runs outside the journal
// lock, while retaining the name lease.
func (s PluginFeed) RemovePlugin() error {
	var installation string
	lease, err := s.AcquirePluginLease()
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return model.NewError(model.CodeConflict, "%s is running or its lease is held; stop it first, then run zalki plugins remove %s again", s.name, s.name)
	}
	if err != nil {
		return err
	}
	defer lease.Close() //nolint:errcheck // Closing releases the installation lease.
	if err := s.locked(func() error {
		p, _, err := s.loadPlugin()
		if errors.Is(err, ErrOldPluginRecord) {
			// An old record cannot be disabled first; the lease proves that no
			// child runs, so it is removed as it is.
			p, err = pluginRecord{ID: p.ID}, nil
		}
		if err != nil {
			return err
		}
		installation = p.ID
		// Prove the leftovers are this installation's before anything goes.
		if err := s.checkInstallArtifacts(p.ID); err != nil {
			return err
		}
		if err := os.Remove(s.pluginPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return journal.SyncDir(s.pluginDir())
	}); err != nil {
		return err
	}
	if installation != "" {
		if err := os.Remove(filepath.Join(s.pluginDir(), "id-"+installation+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(s.pluginDataPath()); err != nil {
		return err
	}
	if err := errors.Join(s.sweepInstallArtifacts(installation), s.sweepPluginInstallFiles()); err != nil {
		return err
	}
	return journal.SyncDir(s.pluginDir())
}

func createPluginInstallation(s PluginFeed, launch *PluginLaunch) error {
	p := pluginRecord{ID: model.NewUUID(), Name: s.name, Launch: launch}
	if err := s.savePluginIdentity(p.ID); err != nil {
		return err
	}
	_, err := s.savePlugin(p)
	return err
}
