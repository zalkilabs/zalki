package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/install"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/platform"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

func archiveBackgroundStopped(ctx context.Context, home string) error {
	binary, err := os.Executable()
	if err != nil {
		return archiveRefusal("background service status is unknown")
	}
	report, err := install.ManagedLoginItem(ctx, install.Request{Action: "inspect", Binary: binary,
		HomeDir: home, StateDir: filepath.Join(home, ".local", "state", "zalki")})
	return archiveBackgroundReport(report, err)
}

func archiveBackgroundReport(report install.Report, err error) error {
	if err != nil || report.OperationPending || report.ManagedState != "missing" ||
		(report.ServiceState != "not_loaded" && report.ServiceState != "unsupported") {
		return archiveRefusal("remove the background login item with zalki uninstall --apply before archiving")
	}
	return nil
}

type archiveLock struct {
	path string
	fd   int
}

type archiveLocks []archiveLock

// Close only: never unlink, truncate, or explicitly unlock an inherited description.
func (held archiveLocks) close() {
	for i := len(held) - 1; i >= 0; i-- {
		_ = platform.CloseLockFile(held[i].fd)
	}
}

func (held archiveLocks) linked() error {
	for _, lock := range held {
		linked, err := platform.Linked(lock.fd, lock.path)
		if err != nil || !linked {
			return archiveRefusal("a lock identity changed")
		}
	}
	return nil
}

func (held *archiveLocks) take(path string) error {
	if len(*held) >= 256 {
		return archiveRefusal("too many activity locks to check safely")
	}
	before, err := os.Lstat(path)
	if err != nil || !privateFile(before) {
		return archiveRefusal("an activity lock is missing or unsafe")
	}
	fd, err := platform.OpenLockFile(path, false)
	if err != nil {
		return archiveRefusal("an activity lock could not be opened without creating it")
	}
	// Attach first so the caller's cleanup closes the FD even on a failed flock.
	*held = append(*held, archiveLock{path: path, fd: fd})
	if err := platform.LockExclusive(fd, true); err != nil {
		return archiveRefusal("a journal, listener or plugin lock is held or cannot be checked")
	}
	linked, err := platform.Linked(fd, path)
	if err != nil || !linked {
		return archiveRefusal("an activity lock changed while it was checked")
	}
	return nil
}

func (p stateArchivePlan) holdActivity(ctx context.Context, ops archiveOps) (archiveLocks, error) {
	held := archiveLocks{}
	// Lifetime/supervisor locks first, journal last: never wait while holding either.
	if err := p.scanActivity(ctx, &held, true); err != nil {
		return held, err
	}
	if err := held.take(filepath.Join(p.source, "lock")); err != nil {
		return held, err
	}
	// Refresh records after acquiring the journal, without reacquiring held lifetimes.
	if err := p.scanActivity(ctx, &held, false); err != nil {
		return held, err
	}
	if err := ops.background(ctx, p.home); err != nil {
		return held, err
	}
	return held, held.linked()
}

func (p stateArchivePlan) scanActivity(ctx context.Context, held *archiveLocks, acquire bool) error {
	return filepath.WalkDir(p.source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return archiveRefusal("an activity path could not be read")
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if strings.HasSuffix(entry.Name(), ".lock") || entry.Name() == "lock" ||
				path == filepath.Join(p.source, "runtimes") || path == filepath.Join(p.source, "plugins") {
				return archiveRefusal("an activity path is a link")
			}
			return nil // Preserve opaque links as bytes; never follow them.
		}
		if path != filepath.Join(p.source, "lock") && strings.HasSuffix(entry.Name(), ".lock") {
			return archiveActivityLock(held, path, acquire)
		}
		if entry.Type()&os.ModeSocket != 0 {
			return archiveSocketStopped(ctx, path)
		}
		if entry.IsDir() {
			return nil
		}
		return archiveRecordStopped(ctx, p.source, path)
	})
}

func archiveActivityLock(held *archiveLocks, path string, acquire bool) error {
	if acquire {
		return held.take(path)
	}
	for _, lock := range *held {
		if lock.path == path {
			return nil
		}
	}
	return archiveRefusal("a new activity lock appeared after inspection")
}

// Stored headers that the current build cannot read remain opaque archive data.
// They are not liveness evidence. Concrete current process/socket probes fail closed.
func archiveRecordStopped(ctx context.Context, root, path string) error {
	dir := filepath.Dir(path)
	if dir != filepath.Join(root, "sessions") && dir != filepath.Join(root, "runtimes") {
		return nil
	}
	if !strings.HasSuffix(path, ".json") {
		return nil
	}
	raw, err := journal.ReadBytes(path, 128<<10)
	if err != nil {
		return archiveRefusal("a current activity record could not be inspected")
	}
	if !archiveHasCurrentHeader(raw) {
		return nil
	}
	var evidence struct {
		Owner    *model.ProcessRef `json:"owner"`
		Process  *model.ProcessRef `json:"process"`
		Endpoint *model.Endpoint   `json:"endpoint"`
	}
	if err := json.Unmarshal(raw, &evidence); err != nil {
		return archiveRefusal("a current activity record is invalid")
	}
	for _, ref := range []*model.ProcessRef{evidence.Owner, evidence.Process} {
		if ref != nil {
			if err := archiveProcessStopped(ref); err != nil {
				return err
			}
		}
	}
	if evidence.Endpoint != nil {
		path := filepath.Clean(evidence.Endpoint.Path)
		if !filepath.IsAbs(path) || !strings.HasPrefix(path, root+string(filepath.Separator)) {
			return archiveRefusal("a listener endpoint cannot be checked inside the selected state")
		}
		return archiveSocketStopped(ctx, path)
	}
	return nil
}

// archiveHasCurrentHeader distinguishes readable current headers from opaque
// archive data. It grants no process or socket liveness on an unreadable header.
func archiveHasCurrentHeader(raw []byte) bool {
	_, err := journal.CheckSchema(raw, 1)
	return err == nil
}

func archiveProcessStopped(ref *model.ProcessRef) error {
	if ref.PID <= 1 || ref.Start == "" {
		return archiveRefusal("a process identity is unknown")
	}
	info, err := procwatch.Read(ref.PID)
	if processAbsent(err) {
		return nil
	}
	if err != nil || info.Start == "" {
		return archiveRefusal("a process could not be inspected")
	}
	if info.Start != ref.Start || info.Zombie {
		return nil
	}
	return archiveRefusal("a session or Zalki runtime is still running")
}

func archiveSocketStopped(ctx context.Context, path string) error {
	info, err := os.Lstat(path)
	if ctx.Err() != nil {
		return archiveRefusal("a listener socket could not be checked")
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return archiveRefusal("a listener socket identity is unsafe or unknown")
	}
	dialer := net.Dialer{Timeout: 200 * time.Millisecond}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err == nil {
		_ = conn.Close()
		return archiveRefusal("a listener socket is still accepting connections")
	}
	if ctx.Err() == nil && (errors.Is(err, os.ErrNotExist) || archiveConnectionRefused(err)) {
		return nil
	}
	return archiveRefusal("a listener socket could not be checked")
}
