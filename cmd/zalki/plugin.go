package main

import (
	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/plugins"
)

const pluginTrustNotice = "External plugins run with your OS user permissions. Access and scope filter what Zalki exposes; they do not restrict filesystem access. A plugin can read and change all of your Zalki state. Run only code you trust."

func newPluginCommand(opts *commandOptions) *cobra.Command {
	group := &cobra.Command{Use: "plugins", Short: "Manage explicitly enabled local plugins", Long: pluginTrustNotice}
	for _, name := range []string{"add", "remove", "configure", "enable", "disable", "status"} {
		group.AddCommand(newPluginOwnerCommand(opts, name))
	}
	group.AddCommand(newPluginCapabilityCommand(opts, "grant"), newPluginCapabilityCommand(opts, "revoke"))
	group.AddCommand(newPluginReadCommand(opts), newPluginInboxReadCommand(opts))
	group.AddCommand(newPluginCheckCommand(opts), newPluginInstallCommand(opts), newPluginListCommand(opts), newPluginShowCommand(opts))
	group.AddCommand(&cobra.Command{
		Use: "run NAME", Short: "Run one installed plugin in the foreground", Long: pluginTrustNotice + "\n\nPlugins MUST exit on stdin EOF within the 2-second grace period and must not daemonize. Helpers must exit with the plugin or not inherit FD 3. Interactive terminal input is unsupported. Plugins speak the one plugin wire: feed offers and ACKs, their own inbox, replies, and the extended send and read grants. The plugin runs as the owner; grants filter only what Zalki exposes.", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			return plugins.Run(cmd.Context(), service.Store, args[0], cmd.OutOrStdout())
		},
	})
	return group
}
