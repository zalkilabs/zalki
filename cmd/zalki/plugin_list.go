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
	return fmt.Sprintf("%s  %s  %s", plainText(status.Name), state, describeAccess(status.Scope, status.Access))
}

// describeAccess is the owner's one-line view of a plugin's access, for
// example "scope all; status, receive; send to 1, read from 0 session(s)".
func describeAccess(scope string, a pluginfeed.PluginAccess) string {
	if scope != "all" {
		scope = "none" // anything else gives no standard access
	}
	var standard []string
	if a.Status {
		standard = append(standard, "status")
	}
	if a.Receive {
		standard = append(standard, "receive")
	}
	if len(standard) == 0 {
		standard = []string{"no standard access"}
	}
	return fmt.Sprintf("scope %s; %s; send to %d, read from %d session(s)", scope, strings.Join(standard, ", "), len(a.Send), len(a.Read))
}
