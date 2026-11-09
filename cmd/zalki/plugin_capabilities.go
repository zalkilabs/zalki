package main

import (
	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
)

func newPluginCapabilityCommand(opts *commandOptions, action string) *cobra.Command {
	var grants bus.PluginCapabilityGrants
	cmd := &cobra.Command{
		Use: action + " NAME", Short: "Manage recorded plugin body/message permissions", Args: cobra.ExactArgs(1),
		Long: "Grant replaces all recorded permissions while disabled; omitted selectors deny. Revoke clears them and disables the installation. Body reads, sends and plugin message transport are unavailable.",
		RunE: func(_ *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			feed := bus.NewPluginFeed(service.Store, args[0])
			if action == "revoke" {
				return feed.RevokePluginCapabilities()
			}
			return feed.GrantPluginCapabilities(grants)
		},
	}
	if action == "grant" {
		cmd.Flags().StringArrayVar(&grants.Read, "read", nil, "exact native session for recorded body-read permission (repeat)")
		cmd.Flags().StringArrayVar(&grants.Send, "send", nil, "exact native recipient for recorded send permission (repeat)")
		cmd.Flags().BoolVar(&grants.InboxRead, "inbox-read", false, "record permission to read the plugin's own inbox")
		cmd.Flags().StringArrayVar(&grants.InboxFrom, "accept-from", nil, "exact native peer allowed to message this plugin (repeat)")
	}
	return cmd
}
