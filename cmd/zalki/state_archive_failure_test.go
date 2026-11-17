package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestStateArchiveCollisionNeverOverwrites(t *testing.T) {
	f := newArchiveFixture(t)
	p := archivePlan(t, f)
	if err := os.Mkdir(p.container, 0700); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(p.container, "keep")
	archiveWrite(t, foreign, []byte("foreign bytes"))
	if err := archiveState(t.Context(), f.request, f.ops); err == nil {
		t.Fatal("ARCHIVE_REUSED_CONTAINER")
	}
	got, err := os.ReadFile(foreign)
	if err != nil || !bytes.Equal(got, []byte("foreign bytes")) {
		t.Fatal("ARCHIVE_OVERWROTE_COLLISION", err)
	}
	if _, err := os.Lstat(f.request.source); err != nil {
		t.Fatal("ARCHIVE_COLLISION_MOVED_SOURCE", err)
	}
}

func TestStateArchiveRefusesChangedSourceAfterConsent(t *testing.T) {
	f := newArchiveFixture(t)
	old := f.request.source + "-original"
	f.request.confirm = func() (bool, error) {
		if err := os.Rename(f.request.source, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(f.request.source, 0700); err != nil {
			t.Fatal(err)
		}
		return true, nil
	}
	if err := archiveState(t.Context(), f.request, f.ops); err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatal("ARCHIVE_ACCEPTED_REPLACEMENT_SOURCE", err)
	}
	if _, err := os.Lstat(old); err != nil {
		t.Fatal("ARCHIVE_LOST_ORIGINAL_SOURCE", err)
	}
}

func TestStateArchiveMissingOrUnsafeLockIsNeverCreated(t *testing.T) {
	for _, mode := range []string{"missing", "link", "directory"} {
		t.Run(mode, func(t *testing.T) {
			f := newArchiveFixture(t)
			path := filepath.Join(f.request.source, "lock")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "link":
				if err := os.Symlink(filepath.Join(f.request.source, "absent-target"), path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := archiveState(t.Context(), f.request, f.ops); err == nil {
				t.Fatal("ARCHIVE_ACCEPTED_UNSAFE_LOCK")
			}
			if mode == "missing" {
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("ARCHIVE_CREATED_MISSING_LOCK", err)
				}
			}
		})
	}
}

func TestStateArchiveProbeFailureRemainsClosed(t *testing.T) {
	f := newArchiveFixture(t)
	f.ops.background = func(context.Context, string) error { return errors.New("fixture inspection unknown") }
	asked := false
	f.request.confirm = func() (bool, error) { asked = true; return true, nil }
	if err := archiveState(t.Context(), f.request, f.ops); err == nil || asked {
		t.Fatal("ARCHIVE_IGNORED_UNKNOWN_SERVICE", err)
	}
	if _, err := os.Lstat(f.request.source); err != nil {
		t.Fatal(err)
	}
}

func TestStateArchiveRenameAndSyncOutcomesAreHonest(t *testing.T) {
	for _, phase := range []string{"pre-rename", "rename-returned-error-after-move", "container-sync", "parent-sync"} {
		t.Run(phase, func(t *testing.T) {
			f := newArchiveFixture(t)
			p := archivePlan(t, f)
			syncs := []string{}
			f.ops.sync = func(path string) error {
				syncs = append(syncs, path)
				if (phase == "container-sync" && path == p.container) || (phase == "parent-sync" && path == p.parent) {
					return syscall.EIO
				}
				return nil
			}
			f.ops.rename = func(source, target string) error {
				if phase == "pre-rename" {
					return syscall.EACCES
				}
				if err := os.Rename(source, target); err != nil {
					return err
				}
				if phase == "rename-returned-error-after-move" {
					return syscall.EIO // Injected late return, not a claimed real rename failure.
				}
				return nil
			}
			err := archiveState(t.Context(), f.request, f.ops)
			assertArchiveFailureOutcome(t, f, p, phase, err, syncs)
		})
	}
}

func assertArchiveFailureOutcome(t *testing.T, f *archiveFixture, p stateArchivePlan, phase string, err error, syncs []string) {
	t.Helper()
	if err == nil || strings.Contains(f.output.String(), "State archived to") {
		t.Fatal("ARCHIVE_CLAIMED_FALSE_SUCCESS", err)
	}
	if phase == "pre-rename" {
		if !strings.Contains(err.Error(), "before a move was observed") {
			t.Fatal("ARCHIVE_PREMOVE_CLASSIFICATION", err)
		}
	} else if !strings.Contains(err.Error(), "unconfirmed") || !strings.Contains(err.Error(), "do not repeat") {
		t.Fatal("ARCHIVE_HID_UNKNOWN_OUTCOME", err)
	}
	if phase == "container-sync" || phase == "parent-sync" {
		if len(syncs) != 2 || syncs[0] != p.container || syncs[1] != p.parent {
			t.Fatal("ARCHIVE_DID_NOT_SYNC_BOTH_NAMESPACES", syncs)
		}
	}
	if _, err := os.Lstat(p.container); err != nil {
		t.Fatal("ARCHIVE_REMOVED_RESERVED_CONTAINER", err)
	}
}
