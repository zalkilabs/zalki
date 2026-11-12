package main

import (
	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func newPluginCapabilityCommand(opts *commandOptions, action string) *cobra.Command {
	var grants pluginfeed.PluginCapabilityGrants
	cmd := &cobra.Command{
		Use: action + " NAME", Short: "Manage recorded plugin body/message permissions", Args: cobra.ExactArgs(1),
		Long: "Grant replaces all recorded permissions while disabled; omitted selectors deny. Revoke clears them and disables the installation. The owner read proxy checks explicit read grants; opt-in v1-read children use the same read grants after metadata ACK. Plugin-origin sends remain unavailable. Sessions named by --receive may store messages at plugin:UUID, and the plugin may answer each such message (at most 8 answers, within 24 hours). Stored, not delivered. The owner inbox-read proxy requires a receive grant. Not acknowledged. Re-run plugins enable NAME once to initialize an existing installation address.",
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			feed, name := pluginfeed.NewPluginFeed(service.Store, args[0]), plainText(args[0])
			if action == "revoke" {
				return pluginDone(cmd, feed.RevokePluginCapabilities(), "Revoked all permissions of %s; it is disabled.", name)
			}
			return pluginDone(cmd, feed.GrantPluginCapabilities(grants), "Recorded permissions for %s.", name)
		},
	}
	if action == "grant" {
		cmd.Flags().StringArrayVar(&grants.Read, "read", nil, "exact session whose received messages it may read, from any sender (repeat)")
		cmd.Flags().StringArrayVar(&grants.Send, "send", nil, "exact native session it may send messages to as this plugin (repeat)")
		cmd.Flags().StringArrayVar(&grants.Receive, "receive", nil, "exact native session that may message this plugin; the plugin reads its own inbox and may answer that session (repeat)")
	}
	return cmd
}
