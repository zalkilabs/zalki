package main

import (
	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func newPluginCapabilityCommand(opts *commandOptions, action string) *cobra.Command {
	var access pluginfeed.PluginAccess
	cmd := &cobra.Command{
		Use: action + " NAME", Short: "Manage a plugin's access", Args: cobra.ExactArgs(1),
		Long: "Grant replaces all access while the plugin is disabled; what is not named is denied. --status and --receive are standard access: they apply to every live session when the scope is all (zalki plugins configure NAME --scope all). With --receive, sessions may message the plugin at plugin:UUID, and it may answer each message (at most 8 answers, within 24 hours). --send and --read are extended access for exact sessions only. Revoke removes all access and disables the plugin.",
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			feed, name := pluginfeed.NewPluginFeed(service.Store, args[0]), plainText(args[0])
			if action == "revoke" {
				return pluginDone(cmd, feed.RevokePluginAccess(), "Revoked all access of %s; it is disabled.", name)
			}
			return pluginDone(cmd, feed.GrantPluginAccess(access), "Recorded access for %s.", name)
		},
	}
	if action == "grant" {
		cmd.Flags().BoolVar(&access.Status, "status", false, "see state and message status of sessions in scope (no text)")
		cmd.Flags().BoolVar(&access.Receive, "receive", false, "sessions in scope may message this plugin, and it may answer them")
		cmd.Flags().StringArrayVar(&access.Read, "read", nil, "exact session whose received messages it may read, from any sender (repeat)")
		cmd.Flags().StringArrayVar(&access.Send, "send", nil, "exact native session it may send messages to unasked, as this plugin (repeat)")
	}
	return cmd
}
