package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

type archiveFixture struct {
	request archiveRequest
	ops     archiveOps
	output  bytes.Buffer
}

func newArchiveFixture(t *testing.T) *archiveFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &archiveFixture{ops: defaultArchiveOps()}
	f.request = archiveRequest{source: filepath.Join(base, "state"), home: filepath.Join(base, "home"),
		config: filepath.Join(base, "config"), origin: "--state", output: &f.output, confirm: func() (bool, error) { return true, nil }}
	for _, path := range []string{f.request.home, f.request.source, filepath.Join(f.request.source, "sessions"), filepath.Join(f.request.source, "messages"), filepath.Join(f.request.source, "plugins")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	archiveWrite(t, filepath.Join(f.request.source, "lock"), []byte{})
	f.ops.background = func(context.Context, string) error { return nil }
	f.ops.now = func() time.Time { return time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC) }
	f.ops.id = func() string { return "00000000-0000-4000-8000-000000000001" }
	return f
}

func archiveWrite(t *testing.T, path string, value []byte) {
	t.Helper()
	if err := os.WriteFile(path, value, 0600); err != nil {
		t.Fatal(err)
	}
}

func archivePlan(t *testing.T, f *archiveFixture) stateArchivePlan {
	t.Helper()
	p, err := prepareStateArchive(f.request, f.ops)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStateArchiveMovesOpaqueBytesAndAsksOnce(t *testing.T) {
	f := newArchiveFixture(t)
	old := []byte("{unsupported rc.7 record; private fixture bytes}\n")
	archiveWrite(t, filepath.Join(f.request.source, "sessions", "old.json"), old)
	archiveWrite(t, filepath.Join(f.request.source, "messages", "pending-unknown.json"), old)
	archiveWrite(t, filepath.Join(f.request.source, "unrecognized-future-data"), old)
	original, err := os.Stat(filepath.Join(f.request.source, "sessions", "old.json"))
	if err != nil {
		t.Fatal(err)
	}
	p := archivePlan(t, f)
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close() //nolint:errcheck // Test-owned terminal.
	defer slave.Close() //nolint:errcheck // Test-owned terminal.
	if _, err := master.Write([]byte("yes\n")); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetIn(slave)
	asked := 0
	f.request.confirm = func() (bool, error) { asked++; return confirmStateArchive(cmd) }
	if err := archiveState(t.Context(), f.request, f.ops); err != nil {
		t.Fatal(err)
	}
	if asked != 1 || strings.Count(f.output.String(), archiveQuestion) != 1 || !strings.Contains(f.output.String(), p.destination) || !strings.Contains(f.output.String(), "start new chats") {
		t.Fatalf("ARCHIVE_QUESTION_AND_NEW_CHATS: %d %q", asked, f.output.String())
	}
	if _, err := os.Lstat(f.request.source); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ARCHIVE_WAS_COPIED_NOT_MOVED: %v", err)
	}
	assertArchivedOpaque(t, p, original, old)
}

func TestStateArchiveDeclineAndEOFDoNotReserveAnything(t *testing.T) {
	for _, name := range []string{"no", "EOF"} {
		t.Run(name, func(t *testing.T) {
			f := newArchiveFixture(t)
			master, slave, err := pty.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer master.Close() //nolint:errcheck // Test-owned terminal.
			defer slave.Close() //nolint:errcheck // Test-owned terminal.
			answer := []byte("n\n")
			if name == "EOF" {
				answer = []byte{4} // Canonical terminal EOF, not an injected false answer.
			}
			if _, err := master.Write(answer); err != nil {
				t.Fatal(err)
			}
			cmd := &cobra.Command{}
			cmd.SetIn(slave)
			f.request.confirm = func() (bool, error) { return confirmStateArchive(cmd) }
			p := archivePlan(t, f)
			if err := archiveState(t.Context(), f.request, f.ops); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(p.container); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ARCHIVE_RESERVED_BEFORE_CONSENT: %v", err)
			}
			if _, err := os.Lstat(f.request.source); err != nil {
				t.Fatal("ARCHIVE_DECLINE_MOVED_STATE", err)
			}
		})
	}
	cmd := &cobra.Command{}
	cmd.SetIn(strings.NewReader("yes\n"))
	if yes, err := confirmStateArchive(cmd); err == nil || yes {
		t.Fatal("ARCHIVE_ACCEPTED_NON_TTY")
	}
}

func TestStateArchiveMissingStateNeverInitializesLogging(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "absent")
	cmd := newRootCommand(base)
	cmd.SetArgs([]string{"--state", state, "state", "archive"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ARCHIVE_CREATED_LOG_OR_STATE: %v", err)
	}
}

func TestStateArchiveRefusesHeldLocksWithoutMoving(t *testing.T) {
	for _, name := range []string{"lock", "cursor-listener-fixture.lock", "plugins/.supervisor.lock", "plugins/demo.lock", "plugins/demo.restart.lock"} {
		t.Run(name, func(t *testing.T) {
			f := newArchiveFixture(t)
			path := filepath.Join(f.request.source, name)
			archiveWrite(t, path, []byte{})
			fd, err := journal.OpenLockFile(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.CloseLockFile(fd) //nolint:errcheck // Test owns this descriptor.
			if err := journal.LockExclusive(fd, true); err != nil {
				t.Fatal(err)
			}
			f.request.confirm = func() (bool, error) { t.Error("ARCHIVE_PROMPTED_WHILE_ACTIVE"); return true, nil }
			if err := archiveState(t.Context(), f.request, f.ops); err == nil {
				t.Fatal("ARCHIVE_IGNORED_HELD_LOCK", name)
			}
			if _, err := os.Lstat(f.request.source); err != nil {
				t.Fatal("ARCHIVE_MOVED_HELD_STATE", err)
			}
		})
	}
}

func TestStateArchiveKeepsJournalAndSupervisorLocksThroughMove(t *testing.T) {
	f := newArchiveFixture(t)
	archiveWrite(t, filepath.Join(f.request.source, "plugins", ".supervisor.lock"), []byte{})
	f.ops.rename = func(source, target string) error {
		for _, name := range []string{"lock", "plugins/.supervisor.lock"} {
			fd, err := journal.OpenLockFile(filepath.Join(source, name), false)
			if err != nil {
				t.Fatal(err)
			}
			err = journal.LockExclusive(fd, true)
			_ = journal.CloseLockFile(fd)
			if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
				t.Fatalf("ARCHIVE_MOVED_WITH_ACTIVE_JOURNAL: %s %v", name, err)
			}
		}
		return os.Rename(source, target)
	}
	if err := archiveState(t.Context(), f.request, f.ops); err != nil {
		t.Fatal(err)
	}
}

func TestStateArchiveRefusesPostConsentActivity(t *testing.T) {
	f := newArchiveFixture(t)
	f.request.confirm = func() (bool, error) {
		path := filepath.Join(f.request.source, "plugins", ".supervisor.lock")
		archiveWrite(t, path, []byte{})
		fd, err := journal.OpenLockFile(path, false)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = journal.CloseLockFile(fd) })
		if err := journal.LockExclusive(fd, true); err != nil {
			t.Fatal(err)
		}
		return true, nil
	}
	if err := archiveState(t.Context(), f.request, f.ops); err == nil {
		t.Fatal("ARCHIVE_IGNORED_POST_CONSENT_ACTIVITY")
	}
	if _, err := os.Lstat(f.request.source); err != nil {
		t.Fatal("ARCHIVE_MOVED_POST_CONSENT_ACTIVE_STATE", err)
	}
}

func TestStateArchiveRefusesLiveProcessAndSocket(t *testing.T) {
	t.Run("process", func(t *testing.T) {
		f := newArchiveFixture(t)
		info, err := procwatch.Read(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(map[string]any{"schema": 1, "owner": model.ProcessRef{PID: info.PID, Start: info.Start}})
		if err != nil {
			t.Fatal(err)
		}
		archiveWrite(t, filepath.Join(f.request.source, "sessions", "current.json"), raw)
		if err := archiveState(t.Context(), f.request, f.ops); err == nil {
			t.Fatal("ARCHIVE_IGNORED_LIVE_PROCESS")
		}
	})
	t.Run("socket", func(t *testing.T) {
		f := newArchiveFixture(t)
		listener, err := net.Listen("unix", filepath.Join(f.request.source, "live.sock"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close() //nolint:errcheck // Test-owned listener.
		if err := archiveState(t.Context(), f.request, f.ops); err == nil {
			t.Fatal("ARCHIVE_IGNORED_LIVE_SOCKET")
		}
	})
}

func assertArchivedOpaque(t *testing.T, p stateArchivePlan, original os.FileInfo, old []byte) {
	t.Helper()
	for _, name := range []string{"sessions/old.json", "messages/pending-unknown.json", "unrecognized-future-data"} {
		got, err := os.ReadFile(filepath.Join(p.destination, name))
		if err != nil || !bytes.Equal(got, old) {
			t.Fatalf("ARCHIVE_OPAQUE_BYTES: %s %v", name, err)
		}
	}
	moved, err := os.Stat(filepath.Join(p.destination, "sessions", "old.json"))
	if err != nil || !os.SameFile(original, moved) || !moved.ModTime().Equal(original.ModTime()) || moved.Mode() != original.Mode() {
		t.Fatalf("ARCHIVE_RECORD_REWRITTEN: %v", err)
	}
}
