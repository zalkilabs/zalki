package main

import (
	"github.com/spf13/cobra"
)

const pluginTrustNotice = "External plugins run with your OS user permissions. Access and scope filter what Zalki exposes; they do not restrict filesystem access. A plugin can read and change all of your Zalki state. Run only code you trust."

func newPluginCommand(opts *commandOptions) *cobra.Command {
	group := &cobra.Command{Use: "plugins", Short: "Manage explicitly enabled local plugins", Long: pluginTrustNotice}
	for _, name := range []string{"add", "remove", "configure", "enable", "disable", "status"} {
		group.AddCommand(newPluginOwnerCommand(opts, name))
	}
	group.AddCommand(newPluginMuteCommand(opts, true), newPluginMuteCommand(opts, false))
	group.AddCommand(newPluginCapabilityCommand(opts, "grant"), newPluginCapabilityCommand(opts, "revoke"))
	group.AddCommand(newPluginReadCommand(opts), newPluginInboxReadCommand(opts))
	group.AddCommand(newPluginCheckCommand(opts), newPluginInstallCommand(opts), newPluginListCommand(opts), newPluginShowCommand(opts))
	group.AddCommand(newPluginUpdateCommand(opts))
	group.AddCommand(newPluginRunCommand(opts), newPluginSuperviseCommand(opts), newPluginRestartCommand(opts), newPluginLogsCommand(opts))
	return group
}
