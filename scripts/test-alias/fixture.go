package main

import (
	"errors"
	"os"
	"path/filepath"
)

type aliasFixture struct {
	root, real, alias, name string
	opened, parent          *os.Root
	identity                os.FileInfo
}

func newAliasFixture() (f aliasFixture, err error) {
	f, err = openAliasFixture()
	if err != nil {
		return f, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.close())
		}
	}()
	if err = f.opened.Mkdir("real", 0700); err != nil {
		return f, err
	}
	if err = f.opened.Symlink(f.real, "a"); err != nil {
		return f, err
	}
	err = f.prove()
	return f, err
}

func openAliasFixture() (f aliasFixture, err error) {
	root, err := os.MkdirTemp("/tmp", "za.")
	if err != nil {
		return f, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, f.closeFailedConstruction(root))
		}
	}()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return f, err
	}
	f.root, f.real, f.alias, f.name = canonical, filepath.Join(canonical, "real"), filepath.Join(canonical, "a"), filepath.Base(canonical)
	f.parent, err = os.OpenRoot(filepath.Dir(canonical))
	if err != nil {
		return f, err
	}
	before, err := f.parent.Lstat(f.name)
	if err != nil || !before.IsDir() {
		return f, errors.Join(err, errors.New("fixture_changed"))
	}
	f.identity = before
	f.opened, err = f.parent.OpenRoot(f.name)
	if err != nil {
		return f, err
	}
	openedIdentity, err := f.opened.Stat(".")
	if err != nil {
		return f, err
	}
	if !os.SameFile(before, openedIdentity) {
		return f, errors.New("fixture_changed")
	}
	return f, nil
}

func (f aliasFixture) current() error {
	info, err := f.parent.Lstat(f.name)
	if err != nil || !info.IsDir() || !os.SameFile(info, f.identity) {
		return errors.Join(errors.New("fixture_changed"), err)
	}
	return nil
}

func (f aliasFixture) prove() error {
	if err := f.current(); err != nil {
		return err
	}
	physical, err := filepath.EvalSymlinks(f.alias)
	if err != nil {
		return err
	}
	if physical != f.real || physical == f.alias {
		return errors.New("temporary alias did not resolve to its owned real directory")
	}
	a, err := os.Stat(f.alias)
	if err != nil {
		return err
	}
	b, err := f.opened.Stat("real")
	if err != nil {
		return err
	}
	if !os.SameFile(a, b) {
		return errors.New("temporary alias identity mismatch")
	}
	return nil
}

func (f aliasFixture) close() (err error) {
	defer func() { err = errors.Join(err, f.opened.Close(), f.parent.Close()) }()
	if err = f.current(); err != nil {
		return err
	}
	if err = f.opened.Remove("a"); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err = f.removeContents(); err != nil {
		return err
	}
	if err = f.current(); err != nil {
		return err
	}
	return unlinkAliasDirectory(f.parent, f.name) // AT_REMOVEDIR refuses files and symlinks at the final boundary.
}

func (f aliasFixture) removeContents() error {
	dir, err := f.opened.Open(".")
	if err != nil {
		return err
	}
	entries, readErr := dir.ReadDir(-1)
	if err := errors.Join(readErr, dir.Close()); err != nil {
		return err
	}
	for _, entry := range entries {
		if err := f.opened.RemoveAll(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (f aliasFixture) closeFailedConstruction(root string) (err error) {
	if f.parent == nil || f.identity == nil {
		err = errors.New("fixture cleanup authority unavailable at " + root)
	} else {
		err = f.current()
		if err == nil {
			err = unlinkAliasDirectory(f.parent, f.name)
		}
	}
	if f.opened != nil {
		err = errors.Join(err, f.opened.Close())
	}
	if f.parent != nil {
		err = errors.Join(err, f.parent.Close())
	}
	return err
}
