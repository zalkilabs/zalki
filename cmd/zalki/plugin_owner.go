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
	cmd := &cobra.Command{
		Use: action + " fake-notifier", Short: "Manage the local reference plugin's explicit grants and enabled state", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "fake-notifier" {
				return errors.New("only fake-notifier is supported")
			}
			service, err := opts.service()
			if err != nil {
				return err
			}
			switch action {
			case "configure":
				return service.Store.ConfigurePluginFeed(sessions)
			case "enable", "disable":
				return service.Store.EnablePluginFeed(action == "enable")
			default:
				return printPluginStatus(cmd, service.Store)
			}
		},
	}
	if action == "configure" {
		cmd.Flags().StringArrayVar(&sessions, "session", nil, "explicit canonical session ID (repeat up to 16 times)")
	}
	return cmd
}

func printPluginStatus(cmd *cobra.Command, store bus.Store) error {
	status, err := store.PluginStatus()
	if err != nil {
		return err
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
		bus.PluginFeedStatus
		Plugin    string `json:"plugin"`
		Outbox    string `json:"outbox"`
		MaxEvents int    `json:"max_events"`
	}{status, "fake-notifier", filepath.Join(store.Dir, "plugin-outbox"), 256})
}
