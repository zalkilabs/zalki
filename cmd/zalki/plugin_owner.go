package main

import (
	"encoding/json"
	"errors"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
)

func newPluginOwnerCommand(opts *commandOptions, action string) *cobra.Command {
	var sessions []string
	var executable string
	cmd := &cobra.Command{
		Use: action + " NAME", Short: "Manage a local plugin's grants and enabled state", Args: func(cmd *cobra.Command, args []string) error {
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
			feed := bus.NewPluginFeed(service.Store, args[0])
			switch action {
			case "add":
				if executable != "" {
					return feed.AddExternalPlugin(executable, args[1:])
				}
				if args[0] != "fake-notifier" || len(args) != 1 {
					return errors.New("external plugin add requires --exec; fake-notifier accepts no arguments")
				}
				return feed.AddPlugin()
			case "remove":
				return feed.RemovePlugin()
			case "configure":
				return feed.ConfigurePluginFeed(sessions)
			case "enable", "disable":
				return feed.EnablePluginFeed(action == "enable")
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

func printPluginStatus(cmd *cobra.Command, store bus.Store, name string) error {
	status, err := bus.NewPluginFeed(store, name).PluginStatus()
	if err != nil {
		return err
	}
	value := struct {
		bus.PluginFeedStatus
		Outbox     string `json:"outbox,omitempty"`
		MaxEvents  int    `json:"max_events,omitempty"`
		PinMatches *bool  `json:"pin_matches,omitempty"`
		PinError   string `json:"pin_error,omitempty"`
	}{PluginFeedStatus: status}
	if name == "fake-notifier" {
		value.Outbox = filepath.Join(status.DataDir, "outbox")
		value.MaxEvents = 256
	}
	if status.Launch != nil {
		err := status.Launch.Verify()
		ok := err == nil
		value.PinMatches = &ok
		if err != nil {
			value.PinError = err.Error()
		}
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}
