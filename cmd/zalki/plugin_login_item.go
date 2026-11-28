package main

import (
	"context"
	"fmt"
	"os/user"

	"github.com/zalkilabs/zalki/internal/install"
)

// Owner lines for the zalki login item. Each one says only what the
// login-item code itself decided or observed (the truth rule).
const (
	loginItemConsentLine    = "It starts now and at every login, and runs until you disable it."
	loginItemFirstLine      = "This adds one macOS background item; turn it off any time in System Settings > General > Login Items & Extensions > Allow in the Background."
	loginItemCombinedAsk    = "Install it, change the zalki login item from %s to %s, and restart background plugins? [y/N]"
	loginItemRelaunchAsk    = "The zalki login item runs %s. Change it to %s and restart background plugins? [y/N]"
	loginItemChangedRefusal = "Nothing was installed: the zalki login item changed while %s was being built (%s). Run the install again."
	loginItemPendingReason  = "an earlier login-item change did not finish; run zalki install in your own terminal to finish it"
	loginItemNotChangedLine = "Background setup was not changed (%s); you can run zalki plugins supervise."
	loginItemDisabledLine   = "Background plugins are disabled for dev.zalki.plugins. Check System Settings > General > Login Items & Extensions > Allow in the Background."
	loginItemIdleLine       = "The zalki login item stays installed and idle. Remove it with zalki uninstall."
	loginItemStartedLine    = "Installed and enabled %s. Start requested. Status: zalki plugins status %s"
	loginItemPartialLine    = "Installed and enabled %s. Background setup failed: %s. Run zalki install in your own terminal to review the login item, or run zalki plugins supervise --state %s."
	loginItemNotSetUpLine   = "Installed and enabled. Background plugins are not set up yet; run zalki plugins install once in your own terminal."
	// Root `zalki install` lines.
	loginItemRootAsk         = "Set up background plugins now? [y/N]"
	loginItemRootNotSetUp    = "Background plugins are not set up yet; run zalki install in your own terminal."
	loginItemRootStarted     = "Background plugins are set up. Start requested."
	loginItemRelaunchedLine  = "The zalki login item now runs %s. Start requested."
	loginItemRootFailed      = "Background setup failed: %s."
	loginItemRemovedNote     = "The zalki login item was removed."
	loginItemWouldRemoveNote = "The zalki login item would be removed."
	loginItemManualRemoval   = "The zalki login item could not be checked, so it was not removed. To remove it by hand, with no other zalki command running: (1) run launchctl print %s and go on only if it shows program %s with the arguments plugins supervise --state %s; (2) run launchctl bootout %s; (3) when launchctl print %s says the service is not found, compare shasum -a 256 %s with \"plist_sha256\" in %s and check that this record names the same program; only when both match, delete the plist and that record; leave %s in place; otherwise leave them all; then run zalki doctor."
)

// loginItems is the one login-item entry point. Tests replace it, so no
// test reaches launchctl or the account's real login item.
var loginItems = install.ManagedLoginItem

// loginItemAccountHome is the OS account home, independent of HOME. Tests
// replace it; a scratch HOME never reaches the login item.
var loginItemAccountHome = func() string {
	current, err := user.Current()
	if err != nil {
		return ""
	}
	return current.HomeDir
}

type loginItemRow int

const (
	rowFirst    loginItemRow = iota + 1 // no item and no job: first setup
	rowOwned                            // our item, same launcher
	rowRelaunch                         // our item, another launcher (B1 != B2)
	rowRefused                          // foreign, changed, unsafe, invalid, unknown or pending
	rowDisabled                         // the OS disabled the label
)

// loginItemTarget is the default-state installation whose login item a
// command may touch. It exists only when nothing chose a custom path.
type loginItemTarget struct {
	binary, home, state string
}

// newLoginItemTarget returns a target only for the default state and config
// paths: a flag, an environment value, an XDG base or a config file entry
// never reaches the login item, even with the same text.
func newLoginItemTarget(opts *commandOptions, state string) *loginItemTarget {
	s := opts.snapshot
	if s == nil || s.Paths.StateDir.Source != "default" || s.Paths.ConfigFile.Source != "default" || opts.state != "" || opts.configFile != "" ||
		opts.home == "" || opts.home != loginItemAccountHome() {
		return nil
	}
	binary, err := installationBinary()
	if err != nil {
		return nil
	}
	return &loginItemTarget{binary: binary, home: opts.home, state: state}
}

func (t *loginItemTarget) request(action string, apply bool) install.Request {
	return install.Request{Action: action, Binary: t.binary, StateDir: t.state, HomeDir: t.home, Apply: apply}
}

// change is a request bound to the record the owner was shown: the backend
// refuses it under its lock, before any write, when the item changed.
func (t *loginItemTarget) change(action string, shown loginItemPlan) install.Request {
	request := t.request(action, true)
	request.ExpectRecord = shown.seen.record
	return request
}

// loginItemPlan is what the owner was shown before the question.
type loginItemPlan struct {
	row      loginItemRow
	launcher string // B1, the owned record's launcher
	binary   string // B2, this zalki
	reason   string // why a refused row changes nothing
	seen     loginItemSeen
}

// loginItemSeen is every inspect input of the decision table. The recheck
// after the build compares all of them, not only the row.
type loginItemSeen struct {
	managed, service, loaded, launcher string
	record                             string // the backend binds a change to it
	pending, failed                    bool
}

// inspect reads the login item without a lock or a write.
func (t *loginItemTarget) inspect(ctx context.Context) loginItemPlan {
	report, err := loginItems(ctx, t.request("inspect", false))
	if err != nil {
		return loginItemPlan{row: rowRefused, binary: t.binary, reason: "its status is unknown", seen: loginItemSeen{failed: true}}
	}
	return classifyLoginItem(report, t.binary)
}

// classifyLoginItem is the decision table. Only a positive observation
// gives a row that changes the item; anything unclear is refused.
func classifyLoginItem(r install.Report, binary string) loginItemPlan {
	plan := loginItemPlan{row: rowRefused, binary: binary, seen: loginItemSeen{
		managed: r.ManagedState, service: r.ServiceState, loaded: r.LoadedIdentity, launcher: r.LoginItemLauncher, pending: r.OperationPending,
		record: r.LoginItemRecord,
	}}
	switch {
	case r.ServiceState == "disabled":
		plan.row = rowDisabled
	case r.OperationPending:
		plan.reason = loginItemPendingReason // names the one repair
	case r.ManagedState == "managed" && r.LoginItemLauncher != "":
		plan.launcher = r.LoginItemLauncher
		switch {
		case r.ServiceState == "loaded" && r.LoadedIdentity != "matches":
			plan.reason = "a different job is loaded"
		case r.ServiceState != "loaded" && r.ServiceState != "not_loaded":
			plan.reason = "its status is unknown"
		case r.LoginItemLauncher == binary:
			plan.row = rowOwned
		default:
			plan.row = rowRelaunch
		}
	case r.ManagedState == "missing" && r.ServiceState == "not_loaded":
		plan.row = rowFirst
	case r.ManagedState == "missing" && r.ServiceState == "loaded":
		plan.reason = "a job is loaded that zalki has no record of"
	default:
		plan.reason = refusedReason(r.ManagedState)
	}
	return plan
}

func refusedReason(state string) string {
	switch state {
	case "foreign":
		return "a login item file exists that zalki did not write"
	case "changed":
		return "the login item file was changed after zalki wrote it"
	case "unsafe":
		return "the login item file is a link or not a private file"
	case "invalid_record":
		return "the login item record is not valid"
	}
	return "its status is unknown"
}

// changes reports whether yes to the question changes the login item.
func (p loginItemPlan) changes() bool {
	return p.row == rowFirst || p.row == rowOwned || p.row == rowRelaunch
}

// disclosure is the lines shown right before the one question.
func (p loginItemPlan) disclosure() []string {
	switch p.row {
	case rowFirst:
		return []string{loginItemConsentLine, loginItemFirstLine}
	case rowOwned, rowRelaunch:
		return []string{loginItemConsentLine}
	case rowDisabled:
		return []string{loginItemDisabledLine}
	case rowRefused:
	}
	return []string{fmt.Sprintf(loginItemNotChangedLine, p.reason)}
}

// question is the one install question: the combined one for a relaunch.
func (p loginItemPlan) question() string {
	if p.row == rowRelaunch {
		return fmt.Sprintf(loginItemCombinedAsk, plainText(p.launcher), plainText(p.binary))
	}
	return pluginReviewQuestion
}

// describe names a state for the refusal line after the build.
func (p loginItemPlan) describe() string {
	switch p.row {
	case rowFirst:
		return "it is not set up"
	case rowOwned, rowRelaunch:
		return "it now runs " + plainText(p.launcher)
	case rowDisabled:
		return "it is disabled in System Settings"
	case rowRefused:
	}
	return p.reason
}

// recheck runs the same read-only look after the build. Any difference
// from what was shown gives one refusal and nothing is published; there is
// never a second question.
func (t *loginItemTarget) recheck(ctx context.Context, shown loginItemPlan, name string) error {
	now := t.inspect(ctx)
	if now == shown {
		return nil
	}
	reason := now.describe()
	if now.row == shown.row && now.reason == shown.reason {
		reason = "its state changed"
	}
	return fmt.Errorf(loginItemChangedRefusal, plainText(name), reason)
}

// apply makes the change the owner agreed to. It returns the reason of a
// failure for the partial-result line, or "" on success.
func (t *loginItemTarget) apply(ctx context.Context, plan loginItemPlan) string {
	action := "install"
	if plan.row == rowRelaunch {
		action = "relaunch"
	}
	if _, err := loginItems(ctx, t.change(action, plan)); err != nil {
		return trimSentence(err)
	}
	return ""
}
