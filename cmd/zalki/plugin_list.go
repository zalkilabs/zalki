package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// pluginRecordName matches plugins/NAME.json; other files there are not records.
var pluginRecordName = regexp.MustCompile(`^([a-z][a-z0-9-]{0,31})\.json$`)

func newPluginListCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List installed plugins and what each may do now", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			entries, err := os.ReadDir(filepath.Join(service.Store.Dir, "plugins"))
			if errors.Is(err, os.ErrNotExist) {
				return pluginDone(cmd, nil, "No plugins are installed.")
			}
			if err != nil {
				return err
			}
			var lines []string
			for _, entry := range entries {
				match := pluginRecordName.FindStringSubmatch(entry.Name())
				if match == nil || !entry.Type().IsRegular() {
					continue
				}
				lines = append(lines, pluginListLine(pluginfeed.NewPluginFeed(service.Store, match[1])))
			}
			if len(lines) == 0 {
				lines = []string{"No plugins are installed."}
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), strings.Join(lines, "\n"))
			return err
		},
	}
}

// pluginListLine is one plain line per plugin. An old record says how to fix it.
func pluginListLine(feed pluginfeed.PluginFeed) string {
	status, err := feed.PluginStatus()
	switch {
	case errors.Is(err, pluginfeed.ErrOldPluginRecord):
		return plainText(status.Name) + "  old record, remove it: " + plainText(err.Error())
	case err != nil:
		return plainText(status.Name) + "  unreadable: " + plainText(err.Error())
	}
	state := "disabled"
	if status.Enabled {
		state = "enabled"
	}
	var g pluginfeed.PluginCapabilityGrants
	if status.Capabilities != nil {
		g = status.Capabilities.PluginCapabilityGrants
	}
	return fmt.Sprintf("%s  %s  changes from %d, receive from %d, send to %d, read from %d session(s)", plainText(status.Name), state, len(status.Sessions), len(g.Receive), len(g.Send), len(g.Read))
}
