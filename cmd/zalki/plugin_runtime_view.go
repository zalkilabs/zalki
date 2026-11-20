package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/procwatch"
)

// The fresh checks behind "observed"; tests replace them. All three only
// read: no file, lock or lease is created.
var (
	readProcess    = procwatch.Read
	controllerLive = supervisorRunning
	currentUID     = os.Getuid
	// backgroundPlatform is the OS whose login item is read; tests set it.
	backgroundPlatform = runtime.GOOS
)

// backgroundLabel is the login item's launchd label.
const backgroundLabel = "dev.zalki.plugins"

const (
	observedRunning = "running"
	observedStopped = "stopped"
	observedUnknown = "unknown"
)

// Owner phrases for the runtime part of a plugins list line. Each one says
// only what the run record and the fresh checks show.
const (
	runPhraseRunning   = "running since %s (pid %d), %d restarts in 10 min"
	runPhraseStarting  = "starting (pid %d)"
	runPhraseBackoff   = "backoff until %s: %s (%d restarts in 10 min)"
	runPhraseCrashed   = "crashed at %s: %d crashes in 10 min (%d restarts in 10 min). Start it again: zalki plugins restart %s"
	runPhraseNeedsYou  = "stopped: %s. Start it again: zalki plugins restart %s"
	runPhraseWaiting   = "waiting until %s: %s"
	runPhraseReinstall = "stopped: %s"
	runPhraseStopped   = "stopped"
	runPhraseUnknown   = "runtime unknown"
)

// Doctor notes about background plugins (bounded, fixed text).
const (
	noteLauncherMissing = "background plugins cannot start: %s is missing"
	noteBrewUninstall   = "raw brew uninstall left the owned plist: zalki uninstall --apply"
)

// pluginRunView is the run{} object of plugins status. plugin_pid is not
// shown: only the host has a verified start fingerprint.
type pluginRunView struct {
	State          string     `json:"state"`
	Observed       string     `json:"observed"`
	PID            int        `json:"pid,omitempty"`
	Since          *time.Time `json:"since,omitempty"`
	Restarts10m    int        `json:"restarts_10m"`
	BackoffStep    int        `json:"backoff_step,omitempty"`
	NextRetryAt    *time.Time `json:"next_retry_at,omitempty"`
	ReasonCode     string     `json:"reason_code,omitempty"`
	LastReason     string     `json:"last_reason,omitempty"`
	LastReasonAt   *time.Time `json:"last_reason_at,omitempty"`
	FirstStartedAt *time.Time `json:"first_started_at,omitempty"`
	InstalledBy    string     `json:"installed_by"`
	RestartSeq     uint64     `json:"restart_seq,omitempty"`
	// The first-start banner state; absent before the first start.
	NotificationState  string `json:"notification_state,omitempty"`
	NotificationReason string `json:"notification_reason,omitempty"`
}

// pluginBackgroundView is the background{} object (status and doctor).
type pluginBackgroundView struct {
	Platform            string  `json:"platform"`
	State               string  `json:"state"`
	ManagedFile         string  `json:"managed_file,omitempty"`
	Label               string  `json:"label"`
	DisplayName         *string `json:"display_name"`
	DisplayNameVerified bool    `json:"display_name_verified"`
	LoadedConfig        string  `json:"loaded_config,omitempty"`
	OperationPending    bool    `json:"operation_pending"`
	Reason              string  `json:"reason,omitempty"`
}

// readRunView reads NAME.run.json without a lock or a write. A missing file
// is a fresh stopped state; a damaged one is kept and shown as unknown.
func readRunView(state, name string, status pluginfeed.PluginFeedStatus, now time.Time) (pluginRunView, pluginRunState, bool) {
	view := pluginRunView{InstalledBy: installedBy(status.Install)}
	run, err := loadRunState(state, name, status.ID)
	if err != nil {
		view.State, view.Observed, view.ReasonCode = observedUnknown, observedUnknown, "run_state_invalid"
		return view, run, false
	}
	view.State, view.Observed = run.State, observeRun(state, run)
	if view.Observed == observedRunning {
		view.PID, view.Since = run.PID, run.Since
	}
	view.Restarts10m = len(recentFailures(run.Restarts, now))
	view.BackoffStep, view.NextRetryAt, view.RestartSeq = run.BackoffStep, run.NextRetryAt, run.RestartSeq
	view.ReasonCode, view.LastReasonAt, view.FirstStartedAt = run.ReasonCode, run.LastReasonAt, run.FirstStartedAt
	view.NotificationState, view.NotificationReason = run.NotificationState, run.NotificationReason
	if run.ReasonCode != "" {
		view.LastReason = reasonText[run.ReasonCode] // validated codes only
	}
	return view, run, true
}

// observeRun is "running" only when all five checks hold now: a live
// controller, a recorded host PID, the process of this user, not a zombie,
// and the start fingerprint the controller saved for the host it started.
// Positive absence of the process is "stopped"; anything else "unknown".
func observeRun(state string, r pluginRunState) string {
	if r.PID == 0 {
		return observedStopped
	}
	info, err := readProcess(r.PID)
	switch {
	case processAbsent(err):
		return observedStopped
	case err != nil || info.PID != r.PID:
		return observedUnknown
	case int(info.UID) != currentUID() || info.Zombie:
		return observedUnknown
	case r.HostStart == "" || info.Start != r.HostStart:
		return observedUnknown
	case !controllerLive(state):
		return observedUnknown
	}
	return observedRunning
}

// installedBy is the install receipt evidence as an enum-like string.
func installedBy(receipt *pluginfeed.PluginInstallReceipt) string {
	if receipt == nil || receipt.InstalledBy == "" {
		return "unknown"
	}
	return receipt.InstalledBy
}

// runPhrase is the runtime part of one list line.
func runPhrase(name string, view pluginRunView, run pluginRunState, ok bool) string {
	if !ok {
		return runPhraseUnknown
	}
	if phrase, live := livePhrase(view, run); live {
		return phrase
	}
	switch {
	case run.State == runBackoff && run.NextRetryAt != nil && len(run.Restarts) == 0:
		return fmt.Sprintf(runPhraseWaiting, run.NextRetryAt.Local().Format("15:04:05"), reasonText[run.ReasonCode])
	case run.State == runBackoff && run.NextRetryAt != nil:
		return fmt.Sprintf(runPhraseBackoff, run.NextRetryAt.Local().Format("15:04:05"), reasonText[run.ReasonCode], view.Restarts10m)
	case run.State == runCrashed:
		return crashedPhrase(name, view, run)
	case run.State == runStopped:
		return runPhraseStopped
	}
	return runPhraseUnknown
}

// crashedPhrase is the phrase for a crashed state.
func crashedPhrase(name string, view pluginRunView, run pluginRunState) string {
	switch {
	case reinstallReasons[run.ReasonCode]:
		return fmt.Sprintf(runPhraseReinstall, reasonText[run.ReasonCode]) // a restart cannot help
	case permanentReasons[run.ReasonCode] || len(run.Restarts) < crashLimit:
		// Stopped for a reason, not by the crash limit.
		return fmt.Sprintf(runPhraseNeedsYou, reasonText[run.ReasonCode], plainText(name))
	case run.LastReasonAt != nil:
		return fmt.Sprintf(runPhraseCrashed, run.LastReasonAt.Local().Format("15:04"), len(run.Restarts), view.Restarts10m, plainText(name))
	}
	return runPhraseUnknown
}

// livePhrase is the phrase for a host that the fresh checks prove alive.
func livePhrase(view pluginRunView, run pluginRunState) (string, bool) {
	switch {
	case view.Observed == observedRunning && run.State == runRunning && run.Since != nil:
		return fmt.Sprintf(runPhraseRunning, run.Since.Local().Format("15:04"), run.PID, view.Restarts10m), true
	case view.Observed == observedRunning && run.State == runStarting:
		return fmt.Sprintf(runPhraseStarting, run.PID), true
	}
	return "", false
}

// backgroundView reads the login item for status and doctor: inspect only
// (no lock, no write), and only for the default installation on macOS.
// launcher is the validated owned record's launcher, or "".
func backgroundView(ctx context.Context, target *loginItemTarget) (pluginBackgroundView, string) {
	view := pluginBackgroundView{Platform: backgroundPlatform, Label: backgroundLabel, State: "unsupported"}
	switch {
	case backgroundPlatform != "darwin":
		view.Reason = "unsupported_platform"
		return view, ""
	case target == nil:
		view.Reason = "custom_state"
		return view, ""
	}
	report, err := loginItems(ctx, target.request("inspect", false))
	if err != nil {
		view.State, view.Reason = "unknown", "inspect_failed"
		return view, ""
	}
	view.State, view.ManagedFile, view.OperationPending = report.ServiceState, report.ManagedState, report.OperationPending
	if report.ServiceState == "loaded" {
		view.LoadedConfig = report.LoadedIdentity
	}
	switch {
	case launcherMissing(report.LoginItemLauncher):
		view.Reason = "launcher_missing"
	case report.ManagedState == "missing" && report.ServiceState == "not_loaded":
		view.Reason = "not_set_up"
	}
	return view, report.LoginItemLauncher
}

// launcherMissing: positive evidence that the owned record's launcher path
// no longer exists. An empty launcher (no validated record) is never missing.
func launcherMissing(launcher string) bool {
	if launcher == "" {
		return false
	}
	_, err := os.Lstat(launcher)
	return errors.Is(err, os.ErrNotExist)
}

// backgroundNotes are doctor's bounded notes about background plugins.
func backgroundNotes(view pluginBackgroundView, launcher string) []string {
	var notes []string
	if view.Reason == "launcher_missing" {
		notes = append(notes, fmt.Sprintf(noteLauncherMissing, plainText(launcher)))
		if view.ManagedFile == "managed" {
			notes = append(notes, noteBrewUninstall)
		}
	}
	if view.State == "disabled" {
		notes = append(notes, loginItemDisabledLine)
	}
	return notes
}

// doctorBackgroundView is doctor's plugins_background object: the status
// background view, the validated launcher (only from a validated owned
// record) and the bounded notes.
type doctorBackgroundView struct {
	pluginBackgroundView
	Launcher       string   `json:"launcher,omitempty"`
	EnabledPlugins int      `json:"enabled_plugins"`
	Notes          []string `json:"notes,omitempty"`
}

func doctorBackground(cmd *cobra.Command, opts *commandOptions, store bus.Store) doctorBackgroundView {
	view, launcher := backgroundView(cmd.Context(), newLoginItemTarget(opts, store.Dir))
	_, enabled, err := installedPlugins(store)
	if err != nil {
		enabled = -1 // unreadable records: the count is unknown
	}
	notes := backgroundNotes(view, launcher)
	notes = append(notes, notifyNotes(store)...)
	return doctorBackgroundView{pluginBackgroundView: view, Launcher: launcher, EnabledPlugins: enabled, Notes: notes}
}
