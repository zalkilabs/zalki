package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// This is a same-user terminal convention, not proof of a biological human.
// Tests replace only this observation; production always checks the input fd.
var pluginAccessTerminal = func(in io.Reader) bool {
	file, ok := in.(*os.File)
	return ok && isInstallTerminal(file)
}

func pluginAccessRecorded(cmd *cobra.Command, err error, name string) error {
	return pluginDone(cmd, err, "Recorded access for %s. Stop any running copy, then start it with: zalki plugins run %s", name, name)
}

func pluginInstallerText(receipt *pluginfeed.PluginInstallReceipt) string {
	if receipt == nil {
		return "unknown (no installation receipt)"
	}
	switch receipt.InstalledBy {
	case "unproven":
		return "an unverified installer"
	case "terminal":
		return "terminal"
	default:
		label := receipt.InstalledByLabel
		if label == "" {
			label = receipt.InstalledBy
		}
		if !simplePluginReviewName(label) {
			label = strconv.QuoteToASCII(label)
		}
		return "session " + label
	}
}

// Enumerate an owner snapshot, then use the shared current membership predicate
// under one view. Counts are advisory snapshot data, never authorization.
func pluginEligibleSnapshot(store bus.Store, status pluginfeed.PluginFeedStatus) string {
	if !status.Enabled || status.Scope != "all" || !status.Access.Status && !status.Access.Receive {
		return "0 eligible in the session snapshot"
	}
	sessions, skipped, err := store.ListWithWarnings()
	if err != nil || skipped != 0 {
		return "eligible snapshot unknown"
	}
	count := 0
	err = bus.WithJournalRead(store, func(view bus.JournalView) error {
		for _, session := range sessions {
			member, err := view.PluginMember(session.ID, status.Name)
			if err != nil {
				return err
			}
			if member {
				count++
			}
		}
		return nil
	})
	if err != nil {
		return "eligible snapshot unknown"
	}
	return fmt.Sprintf("%d eligible in the session snapshot", count)
}

func pluginScopeText(scope string) string {
	if pluginEffectiveScope(scope) == "all" {
		return "all sessions, now and later"
	}
	return "none"
}

func pluginStandardText(access pluginfeed.PluginAccess) string {
	var standard []string
	if access.Status {
		standard = append(standard, "status")
	}
	if access.Receive {
		standard = append(standard, "receive (includes replies to the sender)")
	}
	if len(standard) == 0 {
		return "none"
	}
	return strings.Join(standard, ", ")
}

func pluginStatusForDisplay(store bus.Store, name string) (pluginfeed.PluginFeedStatus, error) {
	status, err := pluginfeed.NewPluginFeed(store, name).PluginStatus()
	if errors.Is(err, pluginfeed.ErrOldPluginRecord) {
		return status, err // backend owns the bounded, shell-quoted recovery route
	}
	if err != nil {
		return status, fmt.Errorf("cannot safely read plugin %s; run zalki doctor", strconv.QuoteToASCII(name))
	}
	if status.ID == "" {
		return status, fmt.Errorf("plugin %s is not installed", strconv.QuoteToASCII(name))
	}
	return status, nil
}

// Stored unknown scope strings are valid fail-closed state, never display text.
func pluginEffectiveScope(scope string) string {
	if scope == "all" {
		return "all"
	}
	return "none"
}
