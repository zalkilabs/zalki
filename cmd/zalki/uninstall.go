package main

import (
	"errors"

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
			report, err := install.Manage(installDeclarations(opts.registry, opts.snapshot.Paths.ConfigFile.Value), install.Request{
				Action: "uninstall", Binary: binary,
				StateDir: service.Store.Dir, HomeDir: opts.home, Apply: apply,
			})
			if err != nil {
				return err
			}
			if target := newLoginItemTarget(opts, service.Store.Dir); target != nil {
				note, err := target.uninstallNote(cmd.Context(), apply)
				if note != "" {
					report.Notes = append(report.Notes, note)
				}
				if err != nil {
					return errors.Join(encodeOutput(cmd, report), err)
				}
			}
			return encodeOutput(cmd, report)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the planned settings changes, keeping backups")
	return cmd
}
