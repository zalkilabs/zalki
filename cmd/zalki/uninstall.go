package main

import (
	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/install"
)

func newUninstallCommand(opts *commandOptions) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use: "uninstall", Short: "Preview removal of managed hooks and MCP entries, preserving message history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			binary, err := installationBinary()
			if err != nil {
				return err
			}
			report, err := install.Manage(opts.registry.Configuration, install.Request{
				Action: "uninstall", Binary: binary,
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
