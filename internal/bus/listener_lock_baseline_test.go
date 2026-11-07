package bus

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
)

func requireRawLockError(t *testing.T, err error, want syscall.Errno, text string) {
	t.Helper()
	got, ok := err.(syscall.Errno) //nolint:errorlint // Raw errno, rather than an unwrap-compatible error, is the baseline contract.
	if !ok || got != want || err.Error() != text || ErrorCodeOf(err) != CodeInternal {
		t.Fatalf("raw lock error = %T %v (code %s), want errno %d, text %q, code internal", err, err, ErrorCodeOf(err), want, text)
	}
}

func TestLockPathErrorsKeepTheirRawTypeAndText(t *testing.T) {
	for _, shape := range []string{"symlink", "directory"} {
		t.Run(shape, func(t *testing.T) {
			store := fixtureStore(t)
			v, _ := register(t, store, "cursor")
			listenerPath := store.listenerLockPath(v.Kind, v.ID)
			globalPath := filepath.Join(store.Dir, "lock")
			if err := os.Remove(globalPath); err != nil {
				t.Fatal(err)
			}
			for _, path := range []string{listenerPath, globalPath} {
				if shape == "symlink" {
					target := filepath.Join(store.Dir, "target")
					if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, path); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			want, text := syscall.ELOOP, "too many levels of symbolic links"
			if shape == "directory" {
				want, text = syscall.EISDIR, "is a directory"
			}
			called := false
			requireRawLockError(t, store.locked(func() error { called = true; return nil }), want, text)
			if called {
				t.Fatal("invalid global lock path permitted domain work")
			}
			fd, err := acquireListenerLock(listenerPath, journal.OpenLockFile)
			if fd != -1 {
				syscall.Close(fd)
				t.Fatalf("invalid lifetime path returned fd %d", fd)
			}
			requireRawLockError(t, err, want, text)
			found, err := store.removeFreeListenerLock(v.Kind, v.ID, false)
			requireRawLockError(t, err, want, text)
			if found || store.lockHeld(v) {
				t.Fatal("invalid lock path reported a free or held lifetime lock")
			}
			if shape == "symlink" {
				raw, err := os.ReadFile(filepath.Join(store.Dir, "target"))
				if err != nil || string(raw) != "unchanged" {
					t.Fatalf("lock open changed symlink target: %q, %v", raw, err)
				}
			}
		})
	}
}

func TestPrivateDirectoryRejectionsKeepTheirTextAndPrecedeLockCreation(t *testing.T) {
	for _, shape := range []string{"symlink", "public permissions", "regular file"} {
		t.Run(shape, func(t *testing.T) {
			store := fixtureStore(t)
			switch shape {
			case "symlink":
				if err := os.Symlink(t.TempDir(), store.Dir); err != nil {
					t.Fatal(err)
				}
			case "public permissions":
				if err := os.Mkdir(store.Dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(store.Dir, 0755); err != nil {
					t.Fatal(err)
				}
			case "regular file":
				if err := os.WriteFile(store.Dir, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err := store.locked(func() error { called = true; return nil })
			want := "state directory must be a private directory, not a symlink: " + store.Dir
			if shape == "regular file" {
				// MkdirAll fails first, before the custom private-directory check.
				pathError, ok := err.(*os.PathError)                                                                                                                                                                //nolint:errorlint // Pin the direct PathError produced by MkdirAll, with no additional wrapper.
				if !ok || pathError.Op != "mkdir" || pathError.Path != store.Dir || pathError.Err != syscall.ENOTDIR || err.Error() != "mkdir "+store.Dir+": not a directory" || ErrorCodeOf(err) != CodeInternal { //nolint:errorlint // Preserve the direct inner errno, not merely errors.Is compatibility.
					t.Fatalf("regular-file directory error = %T %v", err, err)
				}
			} else if err == nil || errors.Unwrap(err) != nil || err.Error() != want || ErrorCodeOf(err) != CodeInternal {
				t.Fatalf("private-directory error = %T %v, want %q", err, err, want)
			}
			if called {
				t.Fatal("invalid state directory permitted domain work")
			}
			if shape != "regular file" {
				requireAbsentLock(t, filepath.Join(store.Dir, "lock"))
			}
		})
	}
}

func TestListenerLinkedDistinguishesMissingAndReplacementInodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listener.lock")
	fd, err := acquireListenerLock(path, journal.OpenLockFile)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if linked, err := lockFileStillLinked(fd, path); err != nil || !linked {
		t.Fatalf("original inode: linked=%v, %v", linked, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if linked, err := lockFileStillLinked(fd, path); err != nil || linked {
		t.Fatalf("missing inode: linked=%v, %v", linked, err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if linked, err := lockFileStillLinked(fd, path); err != nil || linked {
		t.Fatalf("replacement inode: linked=%v, %v", linked, err)
	}
	// Independent open of the replacement proves it is not the locked inode.
	assertRawLockState(t, path, false)
}

func TestListenerUnlinkRetryIsBoundedAndKeepsConflictText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "listener.lock")
	attempts := 0
	open := func(path string, create bool) (int, error) {
		fd, err := journal.OpenLockFile(path, create)
		if err != nil {
			return fd, err
		}
		attempts++
		if err := os.Remove(path); err != nil {
			_ = journal.CloseLockFile(fd)
			return -1, err
		}
		return fd, nil
	}
	fd, err := acquireListenerLock(path, open)
	if fd != -1 {
		syscall.Close(fd)
		t.Fatalf("unlinked lock returned fd %d", fd)
	}
	if attempts != 3 || ErrorCodeOf(err) != CodeConflict || err.Error() != "conflict: listener lock changed while it was being acquired" {
		t.Fatalf("unlink exhaustion: attempts=%d, error=%v; want three attempts and original conflict", attempts, err)
	}
	requireAbsentLock(t, path)
}
