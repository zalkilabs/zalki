package main

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func newPluginOwnerCommand(opts *commandOptions, action string) *cobra.Command {
	var sessions []string
	var executable string
	cmd := &cobra.Command{
		Long: pluginTrustNotice,
		Use:  action + " NAME", Short: "Manage a local plugin's grants and enabled state", Args: func(cmd *cobra.Command, args []string) error {
			if action == "add" {
				if len(args) > 1 && cmd.ArgsLenAtDash() != 1 {
					return errors.New("plugin arguments must follow --")
				}
				return cobra.MinimumNArgs(1)(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			feed, name := pluginfeed.NewPluginFeed(service.Store, args[0]), plainText(args[0])
			switch action {
			case "add":
				if executable == "" {
					return errors.New("plugins add requires --exec ABSOLUTE_PATH; or use zalki plugins install FOLDER")
				}
				return pluginDone(cmd, feed.AddExternalPlugin(executable, args[1:]), "Added %s (disabled).", name)
			case "remove":
				return pluginDone(cmd, feed.RemovePlugin(), "Removed %s.", name)
			case "configure":
				return pluginDone(cmd, feed.ConfigurePluginFeed(sessions), "Configured %s: %d session(s).", name, len(sessions))
			case "enable":
				return pluginDone(cmd, feed.EnablePluginFeed(true), "Enabled %s.", name)
			case "disable":
				return pluginDone(cmd, feed.EnablePluginFeed(false), "Disabled %s.", name)
			default:
				return printPluginStatus(cmd, service.Store, args[0])
			}
		},
	}
	if action == "configure" {
		cmd.Flags().StringArrayVar(&sessions, "session", nil, "explicit canonical session ID (repeat up to 16 times)")
	}
	if action == "add" {
		cmd.Flags().StringVar(&executable, "exec", "", "absolute executable; arguments after -- are literal and visible in process listings")
	}
	return cmd
}

// pluginDone prints one plain line after a successful owner command.
func pluginDone(cmd *cobra.Command, err error, format string, args ...any) error {
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
	return err
}

func printPluginStatus(cmd *cobra.Command, store bus.Store, name string) error {
	status, err := pluginfeed.NewPluginFeed(store, name).PluginStatus()
	if err != nil {
		return err
	}
	value := struct {
		pluginfeed.PluginFeedStatus
		PinMatches        *bool  `json:"pin_matches,omitempty"`
		PinError          string `json:"pin_error,omitempty"`
		CapabilitiesBound *bool  `json:"capabilities_bound,omitempty"`
	}{PluginFeedStatus: status}
	if status.Launch != nil {
		err := status.Launch.Verify()
		ok := err == nil
		value.PinMatches = &ok
		if err != nil {
			value.PinError = err.Error()
		}
	}
	if c := status.Capabilities; c != nil {
		bound := c.Installation == status.ID && status.Launch != nil && c.PinSHA256 == status.Launch.PinSHA256
		value.CapabilitiesBound = &bound
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}
