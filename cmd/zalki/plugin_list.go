package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/statefile"
)

// Only owner record names are considered; unrelated artifacts are never opened.
var pluginRecordName = regexp.MustCompile(`^([a-z][a-z0-9-]{0,31})\.json$`)

func newPluginListCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List installed plugins by claimed identity and current access", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			lines, err := pluginListLines(service.Store)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.Join(lines, "\n"))
			return err
		},
	}
}

func pluginListLines(store bus.Store) ([]string, error) {
	dir := filepath.Join(store.Dir(), "plugins")
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return []string{"No plugins are installed."}, nil
	}
	if err != nil {
		return nil, pluginsFolderProblem(dir, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, pluginsFolderProblem(dir, nil)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, pluginsFolderProblem(dir, err)
	}
	groups := map[string][]string{}
	for _, entry := range entries {
		match := pluginRecordName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		group, line := pluginListEntry(store, match[1], entry.Type().IsRegular())
		groups[group] = append(groups[group], line)
	}
	if len(groups) == 0 {
		return []string{"No plugins are installed."}, nil
	}
	return pluginSortedGroups(groups), nil
}

func pluginListEntry(store bus.Store, name string, regular bool) (string, string) {
	if !regular {
		return "Unreadable plugin records", "  " + name + "  unreadable: " + pluginRecordDetail(store, name, pluginRecordCause(statefile.ErrInvalidFile))
	}
	status, err := pluginfeed.NewPluginFeed(store, name).PluginStatus()
	if errors.Is(err, pluginfeed.ErrPluginRecordUnreadable) {
		return "Unreadable plugin records", "  " + name + "  " + err.Error()
	}
	if err != nil {
		return "Unreadable plugin records", "  " + name + "  unreadable: " + pluginRecordDetail(store, name, pluginRecordCause(err))
	}
	if status.ID == "" {
		return "Unreadable plugin records", "  " + name + "  unreadable: " + pluginRecordDetail(store, name, "it is no longer installed")
	}
	group, version := "Manual plugins (no installation receipt)", "unknown version"
	if status.Install != nil {
		group, version = plainText(status.Install.ID)+" (author's claim)", plainText(status.Install.Version)
	}
	state := "disabled"
	if status.Enabled {
		view, run, ok := readRunView(store.Dir(), name, status, time.Now())
		state = "enabled; " + runPhrase(name, view, run, ok) + notifySuffix(run, ok)
	}
	line := fmt.Sprintf("  %s  %s  %s; scope %s; %s; SEND %d; READ %d; installed by %s",
		name, version, state, pluginEffectiveScope(status.Scope), pluginEligibleSnapshot(store, status), len(status.Access.Send), len(status.Access.Read), pluginInstallerText(status.Install))
	if address := pluginAddress(status); address != "" {
		line += "; address " + address
	}
	return group, line
}

func pluginSortedGroups(groups map[string][]string) []string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	count := len(names)
	for _, group := range groups {
		count += len(group)
	}
	lines := make([]string, 0, count)
	for _, name := range names {
		lines = append(lines, name)
		sort.Strings(groups[name])
		lines = append(lines, groups[name]...)
	}
	return lines
}
