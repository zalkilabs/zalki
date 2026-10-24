package main

import (
	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
)

func newLifecycleCommand(opts *commandOptions, action string) *cobra.Command {
	var apply bool
	short := "Preview removal of managed hooks and MCP entries, preserving message history"
	if action == "upgrade" {
		short = "Preview updating managed configuration to this binary"
	}
	cmd := &cobra.Command{
		Use: action, Short: short, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			binary, err := installationBinary()
			if err != nil {
				return err
			}
			report, err := bus.ManageInstallation(bus.InstallationRequest{
				Action: action, Binary: binary,
				StateDir: service.Store.Dir, HomeDir: opts.home, Apply: apply,
			})
			if err != nil {
				return err
			}
			return encodeOutput(cmd, report)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the planned settings changes, keeping backups")
	return cmd
}
