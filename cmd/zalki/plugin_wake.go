package main

import (
	"context"
	"fmt"
)

// wakeResult is what happened after a committed enable or install: the
// supervisor was already running, a non-killing kickstart was sent, the
// login item is not set up, its state is unknown, or no login item applies
// (a custom state) and nothing runs.
type wakeResult int

const (
	wakeRunning wakeResult = iota + 1
	wakeStarted
	wakeNotSetUp
	wakeUnknown
	wakeManual
)

// wakeSupervisor makes sure a supervisor will start an enabled plugin. It
// sends the non-killing kickstart only with fresh evidence that the login
// item is ours, loaded and matching; it never creates or changes the item.
func wakeSupervisor(ctx context.Context, target *loginItemTarget, state string) wakeResult {
	switch {
	case supervisorRunning(state):
		return wakeRunning
	case target == nil:
		return wakeManual
	}
	plan := target.inspect(ctx)
	switch {
	case plan.row == rowFirst:
		return wakeNotSetUp
	case plan.row != rowOwned || plan.seen.service != "loaded" || plan.seen.loaded != "matches":
		return wakeUnknown
	}
	if _, err := loginItems(ctx, target.request("upgrade", true)); err != nil {
		return wakeUnknown
	}
	return wakeStarted
}

const (
	wakeEnableRunning  = "The running background supervisor starts it."
	wakeEnableStarted  = "Start requested. Status: zalki plugins status %s"
	wakeEnableNotSetUp = "Background plugins are not set up yet; run zalki plugins install once in your own terminal, or start it with: zalki plugins run %s"
	wakeEnableUnknown  = "Background status is unknown; check it with: zalki plugins status %s"
	wakeEnableManual   = "Start it with: zalki plugins run %s"
	wakeInstallRunning = "Installed and enabled %s. The running background supervisor starts it. Status: zalki plugins status %s"
	wakeInstallUnknown = "Installed and enabled %s. Background status is unknown; check it with: zalki plugins status %s"
)

// wakeEnableLine is the second line of a confirmed `plugins enable`.
func wakeEnableLine(result wakeResult, name string) string {
	switch result {
	case wakeRunning:
		return wakeEnableRunning
	case wakeStarted:
		return fmt.Sprintf(wakeEnableStarted, name)
	case wakeNotSetUp:
		return fmt.Sprintf(wakeEnableNotSetUp, name)
	case wakeManual:
		return fmt.Sprintf(wakeEnableManual, name)
	case wakeUnknown:
	}
	return fmt.Sprintf(wakeEnableUnknown, name)
}

// wakeInstallLine is the last line of an install that enabled the plugin
// without changing the login item.
func wakeInstallLine(result wakeResult, name string) string {
	switch result {
	case wakeRunning:
		return fmt.Sprintf(wakeInstallRunning, name, name)
	case wakeStarted:
		return fmt.Sprintf(loginItemStartedLine, name, name)
	case wakeNotSetUp:
		return loginItemNotSetUpLine
	case wakeManual:
		return "Installed. Start it with: zalki plugins run " + name
	case wakeUnknown:
	}
	return fmt.Sprintf(wakeInstallUnknown, name, name)
}
