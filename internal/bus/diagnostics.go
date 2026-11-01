package bus

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type PruneItem struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}
type PruneReport struct {
	DryRun          bool        `json:"dry_run"`
	Sessions        []PruneItem `json:"sessions"`
	Runtimes        []string    `json:"runtimes"`
	DeletedSessions []PruneItem `json:"deleted_sessions"`
	DeletedLocks    []string    `json:"deleted_locks"`
}

// DefaultRetiredRetention is how long a retired session record is kept.
// Message history is not part of this window.
const DefaultRetiredRetention = 7 * 24 * time.Hour

func (s Store) rawSessions() ([]Session, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "sessions", "*.json"))
	if err != nil {
		return nil, err
	}
	items := []Session{}
	for _, path := range files {
		v, err := s.readSession(path)
		if err != nil {
			return nil, err
		}
		// Validate identities before using records as paths in cleanup.
		expected, err := s.sessionPath(v.ID)
		if err != nil || expected != path || !strings.HasPrefix(v.ID, v.Kind+":") {
			return nil, errors.New("session file identity mismatch")
		}
		items = append(items, v)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
}

func (s Store) rawRuntimes() ([]RuntimeInfo, error) {
	files, err := filepath.Glob(filepath.Join(s.Dir, "runtimes", "*.json"))
	if err != nil {
		return nil, err
	}
	items := []RuntimeInfo{}
	for _, path := range files {
		var v RuntimeInfo
		if err := readJSON(path, &v); err != nil {
			return nil, err
		}
		if _, err := UUID(v.ID); err != nil || filepath.Base(path) != v.ID+".json" {
			return nil, errors.New("runtime file identity mismatch")
		}
		items = append(items, v)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].StartedAt < items[j].StartedAt })
	return items, nil
}

// Automatic cleanup requires positive exit evidence, never age or lack of mail.
// Optional exact IDs allow the human to retire legacy records with no owner
// proof; no named/prefix match and no process termination is performed.
// Age only deletes records that are already retired. Prune stays a command:
// running it from a session hook would scan and delete while a client is starting.
func (s Store) Prune(dryRun bool, selected []string) (PruneReport, error) {
	return s.PruneRetired(dryRun, selected, DefaultRetiredRetention)
}

// PruneRetired retires exited sessions and deletes retired records older than
// retiredOlderThan. Messages are kept. A negative window is rejected.
func (s Store) PruneRetired(dryRun bool, selected []string, retiredOlderThan time.Duration) (PruneReport, error) {
	report := PruneReport{DryRun: dryRun, Sessions: []PruneItem{}, Runtimes: []string{}, DeletedSessions: []PruneItem{}, DeletedLocks: []string{}}
	if retiredOlderThan < 0 {
		return report, errors.New("retired retention window must not be negative")
	}
	explicit := map[string]bool{}
	for _, id := range selected {
		if _, err := s.sessionPath(id); err != nil {
			return report, err
		}
		explicit[id] = true
	}
	err := s.locked(func() error {
		// Validate all explicit selections before any changes.
		for id := range explicit {
			if _, err := s.session(id); err != nil {
				return err
			}
		}
		sessions, err := s.rawSessions()
		if err != nil {
			return err
		}
		runtimes, err := s.rawRuntimes()
		if err != nil {
			return err
		}
		now := time.Now()
		for _, v := range sessions {
			// A retirement from an older build can keep its listener lease, which
			// holds the record at the lease schema. Release that leftover without
			// changing the retirement itself. A listener that is still running is
			// fenced by the cleared record. Records already old enough to delete
			// are removed instead; deletion drops the lease with the file.
			if v.Retired {
				if retiredLongEnough(v.UpdatedAt, now, retiredOlderThan) {
					if err := s.deleteRetired(dryRun, v, &report); err != nil {
						return err
					}
					continue
				}
				if v.Listener == nil {
					continue
				}
				report.Sessions = append(report.Sessions, PruneItem{v.ID, "lease_released"})
				if !dryRun {
					releaseListenerLease(&v)
					v.UpdatedAt = timestamp()
					if err := s.saveSession(v); err != nil {
						return err
					}
				}
				continue
			}
			reason := ""
			if explicit[v.ID] {
				reason = "explicit_cleanup"
			} else if s.sessionLiveness(v) == livenessExited {
				reason = "owner_exited"
			}
			if reason == "" {
				continue
			}
			report.Sessions = append(report.Sessions, PruneItem{v.ID, reason})
			if !dryRun {
				v.Retired, v.State, v.RetiredReason, v.Task, v.UpdatedAt = true, StateRetired, reason, "", timestamp()
				releaseListenerLease(&v)
				if err := s.saveSession(v); err != nil {
					return err
				}
			}
		}
		if err := s.reconcileApprovalHolds(dryRun, &report); err != nil {
			return err
		}
		if err := s.deleteOrphanListenerLocks(dryRun, &report); err != nil {
			return err
		}
		for _, v := range runtimes {
			if processLiveness(v.Process) != "exited" {
				continue
			}
			report.Runtimes = append(report.Runtimes, v.ID)
			if !dryRun {
				if err := os.Remove(filepath.Join(s.Dir, "runtimes", v.ID+".json")); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return report, err
}

// retiredLongEnough is true only when updatedAt is strictly older than the
// window. An unparsable timestamp is kept.
func retiredLongEnough(updatedAt string, now time.Time, window time.Duration) bool {
	updated, err := time.Parse(time.RFC3339Nano, updatedAt)
	if err != nil {
		return false
	}
	return updated.Before(now.Add(-window))
}

func (s Store) deleteRetired(dryRun bool, v Session, report *PruneReport) error {
	report.DeletedSessions = append(report.DeletedSessions, PruneItem{v.ID, "retention"})
	path, err := s.sessionPath(v.ID)
	if err != nil {
		return err
	}
	lockRemoved, err := s.removeFreeListenerLock(v.Kind, v.ID, !dryRun)
	if err != nil {
		return err
	}
	if lockRemoved {
		report.DeletedLocks = append(report.DeletedLocks, filepath.Base(s.listenerLockPath(v.Kind, v.ID)))
	}
	if dryRun {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s Store) deleteOrphanListenerLocks(dryRun bool, report *PruneReport) error {
	matches, err := filepath.Glob(filepath.Join(s.Dir, "*-listener-*.lock"))
	if err != nil {
		return err
	}
	sort.Strings(matches)
	for _, path := range matches {
		id, ok := sessionIDFromListenerLock(filepath.Base(path))
		if !ok {
			continue
		}
		sessionPath, err := s.sessionPath(id)
		if err != nil {
			return err
		}
		if _, err := os.Lstat(sessionPath); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		kind, native, _ := strings.Cut(id, ":")
		removed, err := s.removeFreeListenerLock(kind, kind+":"+native, !dryRun)
		if err != nil {
			return err
		}
		if removed {
			report.DeletedLocks = append(report.DeletedLocks, filepath.Base(path))
		}
	}
	return nil
}

func sessionIDFromListenerLock(base string) (string, bool) {
	name := strings.TrimSuffix(base, ".lock")
	kind, native, ok := strings.Cut(name, "-listener-")
	if !ok || kind == "" || native == "" || strings.Contains(kind, "-listener-") {
		return "", false
	}
	return kind + ":" + native, true
}

type SessionDiagnostic struct {
	ID              string              `json:"id"`
	Kind            string              `json:"kind"`
	Name            string              `json:"name,omitempty"`
	State           SessionState        `json:"state"`
	Retired         bool                `json:"retired"`
	RetiredReason   string              `json:"retired_reason,omitempty"`
	Liveness        string              `json:"liveness"`
	Owner           *ProcessRef         `json:"owner,omitempty"`
	UpdatedAt       string              `json:"updated_at"`
	TransportHealth string              `json:"transport_health"`
	ListenerHealth  string              `json:"listener_health,omitempty"`
	HookBuild       string              `json:"hook_build,omitempty"`
	HookBuildStatus string              `json:"hook_build_status"`
	RecoveryHint    string              `json:"recovery_hint,omitempty"`
	Receive         ReceiveCapabilities `json:"receive"`
}

type DiagnosticReport struct {
	Agents             []AgentDiagnostic   `json:"agents"`
	Runtime            RuntimeInfo         `json:"runtime"`
	Runtimes           []RuntimeInfo       `json:"runtimes"`
	UntrackedProcesses []ProcessRef        `json:"untracked_processes"`
	Sessions           []SessionDiagnostic `json:"sessions"`
	Notes              []string            `json:"notes"`
}

type AgentDiagnostic struct {
	Version         string              `json:"installed_version,omitempty"`
	FixtureVersions []string            `json:"fixture_versions,omitempty"`
	Compatibility   string              `json:"compatibility"`
	RecoveryHint    string              `json:"recovery_hint,omitempty"`
	Kind            string              `json:"kind"`
	Label           string              `json:"label"`
	Binary          string              `json:"binary,omitempty"`
	Found           bool                `json:"found"`
	Receive         ReceiveCapabilities `json:"receive"`
}

func (s Store) Diagnostics() (DiagnosticReport, error) {
	report := DiagnosticReport{Runtime: CurrentRuntimeInfo(), Runtimes: []RuntimeInfo{},
		UntrackedProcesses: []ProcessRef{}, Sessions: []SessionDiagnostic{}, Notes: []string{
			"Process liveness does not establish an attached terminal.",
			"Untracked processes have an unknown loaded build; they may predate runtime tracking.",
		}}
	for _, kind := range s.registry().Kinds() {
		adapter, _ := s.registry().Lookup(kind)
		d := adapter.Descriptor()
		binary, err := exec.LookPath(d.Executable)
		diagnostic := AgentDiagnostic{Kind: kind, Label: d.Label, Binary: binary, Found: err == nil, Receive: adapter.ReceiveCapabilities(Session{})}
		annotateClientCompatibility(&diagnostic)
		report.Agents = append(report.Agents, diagnostic)
		report.Notes = append(report.Notes, d.Notes...)
	}
	var sessions []Session
	var runtimes []RuntimeInfo
	err := s.locked(func() error {
		var err error
		sessions, err = s.rawSessions()
		if err != nil {
			return err
		}
		runtimes, err = s.rawRuntimes()
		return err
	})
	if err != nil {
		return report, err
	}
	s.nativeNames(sessions)
	for _, v := range sessions {
		d := SessionDiagnostic{ID: v.ID, Kind: v.Kind, Name: v.Name, State: v.State, Retired: v.Retired,
			RetiredReason: v.RetiredReason, Liveness: s.sessionLiveness(v), Owner: v.Owner,
			UpdatedAt: v.UpdatedAt, HookBuild: v.HookBuild, HookBuildStatus: "unknown"}
		if v.HookBuild != "" && report.Runtime.DiskBuild != "" {
			d.HookBuildStatus = "current"
			if v.HookBuild != report.Runtime.DiskBuild {
				d.HookBuildStatus = "outdated"
			}
		}
		adapter, err := s.registry().Lookup(v.Kind)
		if err != nil {
			return report, err
		}
		d.Receive = adapter.ReceiveCapabilities(v)
		if health, ok := adapter.(healthAdapter); ok {
			d.TransportHealth, d.ListenerHealth = health.Health(hookRuntime{store: s}, v)
		} else {
			d.TransportHealth = "unknown"
		}
		d.RecoveryHint = recoveryHint(d, adapter)
		report.Sessions = append(report.Sessions, d)
	}
	tracked := map[int]bool{}
	for _, v := range runtimes {
		info := runtimeHealth(v)
		if info.ProcessLiveness == "exited" {
			continue
		}
		tracked[info.PID] = true
		report.Runtimes = append(report.Runtimes, info)
	}
	if processes, err := namedProcesses("zalki"); err == nil {
		for _, p := range processes {
			if p.PID != os.Getpid() && !tracked[p.PID] {
				report.UntrackedProcesses = append(report.UntrackedProcesses, ProcessRef{p.PID, p.Start, "untracked_zalki"})
				tracked[p.PID] = true
			}
		}
	} else {
		report.Notes = append(report.Notes, "OS process inspection unavailable: "+err.Error())
	}

	sort.Slice(report.UntrackedProcesses, func(i, j int) bool { return report.UntrackedProcesses[i].PID < report.UntrackedProcesses[j].PID })
	return report, nil
}
