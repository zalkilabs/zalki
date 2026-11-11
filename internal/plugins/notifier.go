package plugins

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// RunNotifier is the trusted reference child. It writes only its private outbox.
func RunNotifier(ctx context.Context, input io.ReadCloser, output io.Writer, outbox string) error {
	// The OS releases raw FD3 at process exit, after all exit handlers.
	if err := pluginfeed.CheckPluginLeaseFD(3); err != nil {
		return err
	}
	if err := journal.LockExclusive(3, true); err != nil {
		return err
	}
	if err := prepareOutbox(outbox); err != nil {
		return err
	}
	if err := writeFrame(output, frame{Hello: &hello{Version: "v0"}}); err != nil {
		return err
	}
	return runNotifierLoop(ctx, input, output, outbox)
}

func runNotifierLoop(ctx context.Context, input io.ReadCloser, output io.Writer, outbox string) error {
	stop := context.AfterFunc(ctx, func() { _ = input.Close() })
	defer stop()
	reader := bufio.NewReaderSize(input, maxFrame+1)
	for {
		value, err := readFrame(reader)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			if ctx.Err() != nil && !errors.Is(err, io.ErrUnexpectedEOF) && (errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe)) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if err := notify(outbox, value); err != nil {
			return err
		}
		if err := writeFrame(output, frame{Ack: value.ID}); err != nil {
			return err
		}
	}
}

func notify(outbox string, value frame) error {
	id, err := model.UUID(value.ID)
	if err != nil || id != value.ID || value.Hello != nil || value.Ack != "" {
		return errors.New("invalid notifier delivery")
	}
	if value.Gap != "" {
		if value.Event != nil || (value.Gap != "enabled" && value.Gap != "retention" && value.Gap != "epoch") {
			return errors.New("invalid notifier gap")
		}
		return nil
	}
	if value.Event == nil || value.Event.ID != value.ID {
		return errors.New("notifier event identity mismatch")
	}
	if err := value.Event.Validate(); err != nil {
		return err
	}
	if value.Event.Type == "session.registered" {
		return nil
	}
	return writeOutbox(outbox, *value.Event)
}

func prepareOutbox(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("notifier outbox must be absolute")
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return err
	}
	if !parent.IsDir() || parent.Mode().Perm()&0077 != 0 || parent.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t.
		return errors.New("outbox requires an existing private parent directory")
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t.
		return errors.New("notifier outbox must be a private directory owned by this user")
	}
	return fsutil.SyncDir(filepath.Dir(path))
}

// Stable output is at most 256 x 4KiB; one atomic staging file is bounded to 4KiB.
func outboxCount(path string) (int, error) {
	dir, err := os.Open(path) //nolint:gosec // G304: caller validated the private outbox path.
	if err != nil {
		return 0, err
	}
	defer dir.Close() //nolint:errcheck // Only reading this bounded directory.
	names, err := dir.Readdirnames(258)
	if err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	count := 0
	for _, name := range names {
		info, err := os.Lstat(filepath.Join(path, name))
		if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
			return 0, errors.New("invalid outbox entry")
		}
		if strings.HasPrefix(name, ".zalki-config-") {
			if err := os.Remove(filepath.Join(path, name)); err != nil {
				return 0, err
			}
			continue
		}
		id, err := model.UUID(strings.TrimSuffix(name, ".json"))
		if err != nil || name != id+".json" {
			return 0, errors.New("unexpected outbox entry")
		}
		count++
	}
	if err := fsutil.SyncDir(path); err != nil {
		return 0, err
	}
	if len(names) == 258 || count > 256 {
		return 0, errors.New("notifier outbox exceeds 256 events or 1MiB")
	}
	return count, nil
}

func writeOutbox(path string, event model.Event) error {
	count, err := outboxCount(path)
	if err != nil {
		return err
	}
	raw, err := journal.EncodeJSON(event, 4096)
	if err != nil {
		return err
	}
	target := filepath.Join(path, event.ID+".json")
	prior, err := journal.ReadBytes(target, 4096)
	if err == nil {
		if !bytes.Equal(prior, raw) {
			return errors.New("outbox event identity has different data")
		}
		return fsutil.WriteAtomic(target, raw)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if count == 256 {
		return fmt.Errorf("notifier outbox is full (256 events / 1MiB): %s; move or remove reviewed event files, then rerun zalki plugins run fake-notifier", path)
	}
	return fsutil.WriteAtomic(target, raw)
}
