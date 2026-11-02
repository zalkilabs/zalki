package bus

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
)

type SessionDiagnostic struct {
	ID              string              `json:"id"`
	Kind            string              `json:"kind"`
	Name            string              `json:"name,omitempty"`
	State           SessionState        `json:"state"`
	Retired         bool                `json:"retired"`
	RetiredReason   string              `json:"retired_reason,omitempty"`
	Liveness        Liveness            `json:"liveness"`
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
		annotateAdapterCompatibility(&diagnostic, adapter)
		report.Agents = append(report.Agents, diagnostic)
		report.Notes = append(report.Notes, d.Notes...)
	}
	var sessions []Session
	var runtimes []RuntimeInfo
	err := s.locked(func() error {
		var err error
		var issues []SessionIssue
		sessions, issues, err = s.scanSessions()
		if len(issues) > 0 {
			report.Notes = append(report.Notes, fmt.Sprintf("Skipped %d unreadable or unsupported session records.", len(issues)))
		}
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
		if health, ok := adapter.(HealthAdapter); ok {
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
