package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func newPluginMuteCommand(opts *commandOptions, muted bool) *cobra.Command {
	action := "unmute"
	if muted {
		action = "mute"
	}
	var actor string
	cmd := &cobra.Command{Use: action + " NAME", Short: "Control a plugin for your authenticated session", Args: cobra.ExactArgs(1), Long: "Affects only your authenticated session. Unmute keeps owner and ZALKI_PLUGINS restrictions. These controls filter Zalki APIs, not OS access. Active plugins means authorized access, not proof of a running process. After upgrading to rc.8, restart existing sessions to include them in plugin access.", RunE: func(cmd *cobra.Command, args []string) error {
		if !cmd.Flags().Changed("actor") {
			actor = os.Getenv("ZALKI_ACTOR")
		}
		if model.ValidateSessionAddress(actor) != nil || model.IsPluginInboxAddress(actor) {
			return errors.New("set ZALKI_ACTOR or --actor to your own canonical session ID")
		}
		key := os.Getenv("ZALKI_CAPABILITY")
		if key == "" {
			return errors.New("set ZALKI_CAPABILITY to your own private SessionStart capability for this command")
		}
		service, err := opts.service()
		if err != nil {
			return err
		}
		if err := bus.SetSessionPlugin(service.Store, actor, key, args[0], muted); err != nil {
			return err
		}
		if muted {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Muted %s for this session.\n", args[0])
		} else {
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Unmuted %s for this session. Owner and ZALKI_PLUGINS restrictions still apply.\n", args[0])
		}
		return err
	}}
	cmd.Flags().StringVar(&actor, "actor", "", "your own canonical session ID (defaults to ZALKI_ACTOR)")
	return cmd
}
