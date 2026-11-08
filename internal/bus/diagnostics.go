package bus

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

type DiagnosticReport struct {
	Agents             []AgentDiagnostic         `json:"agents"`
	Runtime            RuntimeInfo               `json:"runtime"`
	Runtimes           []RuntimeInfo             `json:"runtimes"`
	UntrackedProcesses []model.ProcessRef        `json:"untracked_processes"`
	Sessions           []model.SessionDiagnostic `json:"sessions"`
	Notes              []string                  `json:"notes"`
}

type AgentDiagnostic struct {
	Version         string                    `json:"installed_version,omitempty"`
	FixtureVersions []string                  `json:"fixture_versions,omitempty"`
	Compatibility   string                    `json:"compatibility"`
	RecoveryHint    string                    `json:"recovery_hint,omitempty"`
	Kind            string                    `json:"kind"`
	Label           string                    `json:"label"`
	Binary          string                    `json:"binary,omitempty"`
	Found           bool                      `json:"found"`
	Receive         model.ReceiveCapabilities `json:"receive"`
}

func (s Store) Diagnostics(ctx context.Context) (DiagnosticReport, error) {
	report := DiagnosticReport{Runtime: CurrentRuntimeInfo(), Runtimes: []RuntimeInfo{},
		UntrackedProcesses: []model.ProcessRef{}, Sessions: []model.SessionDiagnostic{}, Notes: []string{
			"Process liveness does not establish an attached terminal.",
			"Untracked processes have an unknown loaded build; they may predate runtime tracking.",
		}}
	s.diagnoseAgents(ctx, &report)
	var sessions []model.Session
	var runtimes []RuntimeInfo
	err := s.readLocked(func() error {
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
	if err := s.diagnoseSessions(&report, sessions); err != nil {
		return report, err
	}
	diagnoseRuntimes(&report, runtimes)
	return report, nil
}

// diagnoseAgents reports each registered client's executable and compatibility.
func (s Store) diagnoseAgents(ctx context.Context, report *DiagnosticReport) {
	for _, kind := range s.registry().Kinds() {
		adapter, _ := s.registry().Lookup(kind)
		d := adapter.Descriptor()
		binary, err := exec.LookPath(d.Executable)
		diagnostic := AgentDiagnostic{Kind: kind, Label: d.Label, Binary: binary, Found: err == nil, Receive: adapter.ReceiveCapabilities(model.Session{})}
		annotateClientCompatibility(ctx, &diagnostic)
		annotateAdapterCompatibility(&diagnostic, adapter)
		report.Agents = append(report.Agents, diagnostic)
		report.Notes = append(report.Notes, d.Notes...)
	}
}

func (s Store) diagnoseSessions(report *DiagnosticReport, sessions []model.Session) error {
	for _, v := range sessions {
		d := model.SessionDiagnostic{ID: v.ID, Kind: v.Kind, Name: v.Name, State: v.State, Retired: v.Retired,
			RetiredReason: v.RetiredReason, Liveness: s.sessionLiveness(v), Owner: v.Owner,
			UpdatedAt: v.UpdatedAt, HookBuild: v.HookBuild, HookBuildStatus: hookBuildStatus(v.HookBuild, report.Runtime.DiskBuild)}
		adapter, err := s.registry().Lookup(v.Kind)
		if err != nil {
			return err
		}
		d.Receive = adapter.ReceiveCapabilities(v)
		if health, ok := adapter.(agent.HealthAdapter); ok {
			d.TransportHealth, d.ListenerHealth = health.Health(hookRuntime{store: s}, v)
		} else {
			d.TransportHealth = "unknown"
		}
		d.RecoveryHint = recoveryHint(d, adapter)
		report.Sessions = append(report.Sessions, d)
	}
	return nil
}

// hookBuildStatus compares a session's hook build with this binary on disk.
func hookBuildStatus(hookBuild, diskBuild string) string {
	switch {
	case hookBuild == "" || diskBuild == "":
		return "unknown"
	case hookBuild != diskBuild:
		return "outdated"
	default:
		return "current"
	}
}

// diagnoseRuntimes lists live tracked Zalki processes, then any other running
// zalki process as untracked.
func diagnoseRuntimes(report *DiagnosticReport, runtimes []RuntimeInfo) {
	tracked := map[int]bool{}
	for _, v := range runtimes {
		info := runtimeHealth(v)
		if info.ProcessLiveness == "exited" {
			continue
		}
		tracked[info.PID] = true
		report.Runtimes = append(report.Runtimes, info)
	}
	if processes, err := procwatch.Named("zalki"); err == nil {
		for _, p := range processes {
			if p.PID != os.Getpid() && !tracked[p.PID] {
				report.UntrackedProcesses = append(report.UntrackedProcesses, model.ProcessRef{PID: p.PID, Start: p.Start, Role: "untracked_zalki"})
				tracked[p.PID] = true
			}
		}
	} else {
		report.Notes = append(report.Notes, "OS process inspection unavailable: "+err.Error())
	}
	sort.Slice(report.UntrackedProcesses, func(i, j int) bool { return report.UntrackedProcesses[i].PID < report.UntrackedProcesses[j].PID })
}
