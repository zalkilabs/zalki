package bus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
)

// Version is set by release builds with -X.
var Version = "0.1.0-experimental"

type RuntimeInfo struct {
	ID              string      `json:"id,omitempty"`
	Role            string      `json:"role"`
	Version         string      `json:"version"`
	Binary          string      `json:"binary"`
	PID             int         `json:"pid"`
	Process         *ProcessRef `json:"process,omitempty"`
	StartedAt       string      `json:"started_at"`
	LoadedBuild     string      `json:"loaded_build"`
	DiskBuild       string      `json:"disk_build,omitempty"`
	NeedsRestart    bool        `json:"needs_restart"`
	ProcessLiveness string      `json:"process_liveness"`
	Actors          []string    `json:"actors,omitempty"`
	BuildError      string      `json:"build_error,omitempty"`
}

type fileIdentity struct {
	Device, Inode           uint64
	Size, Modified, Changed int64
}
type digestCacheEntry struct {
	identity fileIdentity
	digest   string
}

var binaryDigests = struct {
	sync.Mutex
	entries map[string]digestCacheEntry
}{entries: map[string]digestCacheEntry{}}

func binaryDigest(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the running zalki executable
	if err != nil {
		return "", err
	}
	defer f.Close() //nolint:errcheck // Read-only executable digest has no pending writes.
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("binary is not a regular file")
	}
	stat := info.Sys().(*syscall.Stat_t)
	identity := fileIdentity{uint64(stat.Dev), stat.Ino, info.Size(), info.ModTime().UnixNano(), changedNS(stat)} // #nosec G115 -- Preserve opaque device identity; Darwin's dev_t is signed, Linux's is unsigned.
	binaryDigests.Lock()
	defer binaryDigests.Unlock()
	if previous, ok := binaryDigests.entries[path]; ok && previous.identity == identity {
		return previous.digest, nil
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	binaryDigests.entries[path] = digestCacheEntry{identity, digest}
	return digest, nil
}

// Capture before an atomic binary replacement can change the on-disk hash.
var currentExecutable = captureExecutable()

type executableSnapshot struct {
	info        RuntimeInfo
	loadedBuild string
}

func captureExecutable() executableSnapshot {
	info := RuntimeInfo{Role: "cli", Version: Version, PID: os.Getpid(), StartedAt: timestamp()}
	path, err := os.Executable()
	if err == nil {
		info.Binary = path
		info.LoadedBuild, err = binaryDigest(path)
	}
	if err != nil {
		info.BuildError = err.Error()
	}
	if p, err := readProcess(os.Getpid()); err == nil {
		info.Process = &ProcessRef{PID: p.PID, Start: p.Start, Role: "zalki"}
	}
	return executableSnapshot{info: info, loadedBuild: info.LoadedBuild}
}

func runtimeHealth(info RuntimeInfo) RuntimeInfo {
	info.ProcessLiveness = processLiveness(info.Process)
	info.DiskBuild, info.NeedsRestart = "", false
	if digest, err := binaryDigest(info.Binary); err == nil {
		info.DiskBuild = digest
		info.NeedsRestart = info.LoadedBuild != "" && digest != info.LoadedBuild
	} else {
		info.BuildError = err.Error()
	}
	return info
}

func CurrentRuntimeInfo() RuntimeInfo { return runtimeHealth(currentExecutable.info) }

func BuildVersion() string {
	build := currentExecutable.loadedBuild
	if len(build) >= 12 {
		return Version + "+" + build[:12]
	}
	return Version
}

type RuntimeLease struct {
	store  Store
	info   RuntimeInfo
	mu     sync.Mutex
	closed bool
}

func (s Store) TrackRuntime(role, actor string) (*RuntimeLease, error) {
	info := currentExecutable.info
	info.ID, info.Role = newID(), role
	if actor != "" {
		if _, err := s.sessionPath(actor); err != nil {
			return nil, err
		}
		info.Actors = []string{actor}
	}
	err := s.locked(func() error {
		if err := privateDir(filepath.Join(s.Dir, "runtimes")); err != nil {
			return err
		}
		return writeJSON(filepath.Join(s.Dir, "runtimes", info.ID+".json"), info)
	})
	return &RuntimeLease{store: s, info: info}, err
}

func (r *RuntimeLease) Info() RuntimeInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	info := r.info
	info.Actors = append([]string(nil), info.Actors...)
	return runtimeHealth(info)
}

// Labels are best effort after a successful authenticated tool call. Never
// change a sent message's receipt because ancillary diagnostic IO failed.
func (r *RuntimeLease) ObserveActor(actor string) {
	if actor == "" {
		return
	}
	if _, err := r.store.sessionPath(actor); err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	for _, existing := range r.info.Actors {
		if existing == actor {
			return
		}
	}
	if len(r.info.Actors) >= 64 {
		return
	}
	updated := r.info
	updated.Actors = append(append([]string(nil), r.info.Actors...), actor)
	sort.Strings(updated.Actors)
	if err := r.store.locked(func() error {
		return writeJSON(filepath.Join(r.store.Dir, "runtimes", r.info.ID+".json"), updated)
	}); err == nil {
		r.info = updated
	}
}

func (r *RuntimeLease) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	return r.store.locked(func() error {
		err := os.Remove(filepath.Join(r.store.Dir, "runtimes", r.info.ID+".json"))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// Keep ordinary messaging responses compact; doctor contains full process,
// path, actor, and startup diagnostics.
type RuntimeStatus struct {
	PID          int    `json:"pid"`
	LoadedBuild  string `json:"loaded_build"`
	DiskBuild    string `json:"disk_build,omitempty"`
	NeedsRestart bool   `json:"needs_restart"`
	BuildError   string `json:"build_error,omitempty"`
}

func (s *Service) RuntimeStatus() RuntimeStatus {
	info := s.RuntimeInfo()
	status := RuntimeStatus{PID: info.PID, LoadedBuild: info.LoadedBuild, NeedsRestart: info.NeedsRestart, BuildError: info.BuildError}
	if info.NeedsRestart {
		status.DiskBuild = info.DiskBuild
	}
	return status
}

func (s *Service) RuntimeInfo() RuntimeInfo {
	if s.Runtime != nil {
		return s.Runtime.Info()
	}
	return CurrentRuntimeInfo()
}
