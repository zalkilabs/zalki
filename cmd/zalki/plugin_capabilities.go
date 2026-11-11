package main

import (
	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func newPluginCapabilityCommand(opts *commandOptions, action string) *cobra.Command {
	var grants pluginfeed.PluginCapabilityGrants
	cmd := &cobra.Command{
		Use: action + " NAME", Short: "Manage recorded plugin body/message permissions", Args: cobra.ExactArgs(1),
		Long: "Grant replaces all recorded permissions while disabled; omitted selectors deny. Revoke clears them and disables the installation. The owner read proxy checks explicit read grants; opt-in v1-read children use the same read grants after metadata ACK. Plugin-origin sends remain unavailable. Native peers allowed by accept-from may store messages at plugin:UUID. Stored, not delivered. The owner inbox-read proxy requires --inbox-read. Not acknowledged. Re-run plugins enable NAME once to initialize an existing installation address.",
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
		cmd.Flags().StringArrayVar(&grants.Send, "send", nil, "exact native recipient for recorded send permission (repeat)")
		cmd.Flags().BoolVar(&grants.InboxRead, "inbox-read", false, "record permission to read the plugin's own inbox")
		cmd.Flags().StringArrayVar(&grants.InboxFrom, "accept-from", nil, "exact native peer allowed to message this plugin (repeat)")
	}
	return cmd
}
