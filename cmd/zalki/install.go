package main

import (
	"fmt"

	"github.com/zalkilabs/zalki/internal/install"

	"github.com/spf13/cobra"
)

func newSetupCommand(opts *commandOptions) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use: "setup", Short: "Write agent configuration drafts for review", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return writeConfigurationDrafts(cmd, opts, out) },
	}
	cmd.Flags().StringVar(&out, "out", "generated", "configuration draft directory")
	return cmd
}

func newInstallCommand(opts *commandOptions) *cobra.Command {
	var out string
	var apply bool
	cmd := &cobra.Command{
		Use: "install", Short: "Draft or apply agent configuration, keeping backups", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !apply {
				return writeConfigurationDrafts(cmd, opts, out)
			}
			return applyConfiguration(cmd, opts)
		},
	}
	cmd.Flags().StringVar(&out, "out", "generated", "configuration draft directory")
	cmd.Flags().BoolVar(&apply, "apply", false, "apply registered adapter configuration to your user settings, keeping backups")
	return cmd
}

func applyConfiguration(cmd *cobra.Command, opts *commandOptions) error {
	service, err := opts.service()
	if err != nil {
		return err
	}
	binary, err := installationBinary()
	if err != nil {
		return err
	}
	paths, err := install.Apply(opts.registry.Configuration, binary, service.Store.Dir, opts.home)
	if err != nil {
		return err
	}
	for _, path := range paths {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Updated:", path); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Existing settings were preserved and backed up. Review/trust integrations in each configured client, then launch normally. Check doctor for adapter and listener requirements.")
	return err
}

func writeConfigurationDrafts(cmd *cobra.Command, opts *commandOptions, out string) error {
	service, err := opts.service()
	if err != nil {
		return err
	}
	binary, err := installationBinary()
	if err != nil {
		return err
	}
	if err := install.WriteSetup(opts.registry.Configuration, binary, service.Store.Dir, out, opts.home); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Configuration drafts written to", out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "To apply after review: zalki install --apply")
	return err
}
