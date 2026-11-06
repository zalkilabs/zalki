package bus

import (
	"errors"
	"os"
	"syscall"

	"github.com/zalkilabs/zalki/internal/procwatch"
)

// PID alone is insufficient: a later process can reuse it. Start identifies the
// original OS process; Role distinguishes an interactive client from a backend.
type ProcessRef struct {
	PID   int    `json:"pid"`
	Start string `json:"start"`
	Role  string `json:"role"`
}

type processInfo = procwatch.Info

func readProcess(pid int) (processInfo, error) { return procwatch.Read(pid) }

func namedProcesses(name string) ([]processInfo, error) { return procwatch.Named(name) }

func processLiveness(ref *ProcessRef) Liveness { return probeProcess(ref, readProcess) }

func probeProcess(ref *ProcessRef, read func(int) (processInfo, error)) Liveness {
	if ref == nil || ref.PID <= 1 || ref.Start == "" {
		return LivenessUnknown
	}
	current, err := read(ref.PID)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return LivenessExited
	}
	if err != nil || current.Start == "" {
		return LivenessUnknown // Permission and inspection failures are not exit evidence.
	}
	if current.Start != ref.Start || current.UID != uint32(os.Getuid()) || current.Zombie { //nolint:gosec // G115: getuid returns a Unix uid_t, which fits uint32.
		return LivenessExited
	}
	return LivenessAlive
}

// Only trusted hooks supply process ownership; peer/model PIDs are not accepted.
func (s Store) nativeOwner(kind string) *ProcessRef {
	adapter, err := s.registry().Lookup(kind)
	if err != nil {
		return nil
	}
	if !s.tracksOwnProcess(kind) {
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

// rememberOwner keeps the recorded owner process and hook build of a session up
// to date. Under the journal lock it does this, in this order: load the session
// (a missing one is not an error), update the owner if the session is not
// retired or this is a refresh, always update the hook build, and save only if
// something changed.
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
			changed = s.updateOwner(&v, kind, refresh)
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

// updateOwner decides the owner of a session and reports whether it changed v.
// A shared helper or daemon is not this session, so an adapter that does not
// track its own process never gets an owner, and a refresh drops one that an
// older build recorded. Otherwise an owner is looked up when there is none or
// on refresh: a found one is recorded, and if none is found a refresh drops a
// recorded owner that has exited. A trusted new prompt proves resumed activity
// even when the OS cannot supply a replacement owner fingerprint.
func (s Store) updateOwner(v *Session, kind string, refresh bool) bool {
	if !s.tracksOwnProcess(kind) {
		if refresh && v.Owner != nil {
			v.Owner = nil
			return true
		}
		return false
	}
	if v.Owner != nil && !refresh {
		return false
	}
	if owner := s.nativeOwner(kind); owner != nil {
		v.Owner = owner
		return true
	}
	if refresh && processLiveness(v.Owner) == LivenessExited {
		v.Owner = nil
		return true
	}
	return false
}
