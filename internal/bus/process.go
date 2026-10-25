package bus

import (
	"errors"
	"os"
	"syscall"
)

// PID alone is insufficient: a later process can reuse it. Start identifies the
// original OS process; Role distinguishes an interactive client from a backend.
type ProcessRef struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
	Role  string `json:"role"`
}

type processInfo struct {
	PID, PPID int
	Start     string
	Name      string
	UID       uint32
	Zombie    bool
}

func processLiveness(ref *ProcessRef) string { return probeProcess(ref, readProcess) }

func probeProcess(ref *ProcessRef, read func(int) (processInfo, error)) string {
	if ref == nil || ref.PID <= 1 || ref.Start == "" {
		return "unknown"
	}
	current, err := read(ref.PID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return "exited"
	}
	if err != nil || current.Start == "" {
		return "unknown" // Permission and inspection failures are not exit evidence.
	}
	if current.Start != ref.Start || current.UID != uint32(os.Getuid()) || current.Zombie { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return "exited"
	}
	return "alive"
}

// Only trusted hooks supply process ownership; peer/model PIDs are not accepted.
func (s Store) nativeOwner(kind string) *ProcessRef {
	adapter, err := s.registry().Lookup(kind)
	if err != nil {
		return nil
	}
	return ownerFrom(adapter.Descriptor(), os.Getppid(), readProcess)
}

func ownerFrom(adapter AdapterDescriptor, pid int, read func(int) (processInfo, error)) *ProcessRef {
	for range 32 {
		if pid <= 1 {
			break
		}
		p, err := read(pid)
		if err != nil || p.UID != uint32(os.Getuid()) { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
			break
		}
		for _, name := range adapter.ProcessNames {
			if p.Name == name {
				return &ProcessRef{PID: p.PID, Start: p.Start, Role: adapter.OwnerRole}
			}
		}
		if p.PPID == pid {
			break
		}
		pid = p.PPID
	}
	return nil
}

func (s Store) rememberOwner(id, kind string, refresh bool) error {
	return s.locked(func() error {
		v, err := s.session(id)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		changed := false
		if !v.Retired || refresh {
			if v.Owner == nil || refresh {
				if owner := s.nativeOwner(kind); owner != nil {
					v.Owner, changed = owner, true
				} else if refresh && processLiveness(v.Owner) == "exited" {
					// A trusted new prompt proves resumed activity even when the
					// OS cannot supply a replacement owner fingerprint.
					v.Owner, changed = nil, true
				}
			}
		}
		build := currentExecutable.loadedBuild
		if v.HookBuild != build {
			v.HookBuild, changed = build, true
		}
		if changed {
			return s.saveSession(v)
		}
		return nil
	})
}
