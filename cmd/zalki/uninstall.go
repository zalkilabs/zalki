package main

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/install"
)

func newUninstallCommand(opts *commandOptions) *cobra.Command {
	var f changeFlags
	cmd := &cobra.Command{
		Use: "uninstall", Short: "Remove the Zalki hooks and MCP entries from your clients, keeping message history", Args: cobra.NoArgs,
		Long: "Lists the files Zalki will change, asks one question, then removes only the entries Zalki wrote and keeps a backup of each file. " +
			"Use --yes to skip the question, or --dry-run to print what would change.",
		RunE: func(cmd *cobra.Command, _ []string) error { return runUninstall(cmd, opts, f) },
	}
	addChangeFlags(cmd, &f)
	return cmd
}

func runUninstall(cmd *cobra.Command, opts *commandOptions, f changeFlags) error {
	service, err := opts.service()
	if err != nil {
		return err
	}
	request, err := installRequest(opts, service, "uninstall")
	if err != nil {
		return err
	}
	declare := installDeclarations(opts.registry, opts.snapshot.Paths.ConfigFile.Value)
	if !f.dryRun && !f.yes {
		preview, err := install.Manage(declare, request)
		if err != nil {
			return err
		}
		if ok, err := confirmChange(cmd, "uninstall", preview, opts.home); err != nil || !ok {
			return err
		}
	}
	request.Apply = !f.dryRun
	report, err := install.Manage(declare, request)
	if err != nil {
		return err
	}
	if target := newLoginItemTarget(opts, service.Store.Dir()); target != nil {
		note, err := target.uninstallNote(cmd.Context(), request.Apply)
		if note != "" {
			report.Notes = append(report.Notes, note)
		}
		if err != nil {
			return errors.Join(encodeOutput(cmd, report), err)
		}
	}
	return errors.Join(encodeOutput(cmd, report), skippedClientsError(report))
}
