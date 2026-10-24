package main

import "github.com/spf13/cobra"

func newListCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List registered sessions as JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			sessions, err := service.Store.List()
			if err != nil {
				return err
			}
			return encodeOutput(cmd, sessions)
		},
	}
}

func newDoctorCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: "Report builds, processes, and receive health as JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			report, err := service.Store.Diagnostics()
			if err != nil {
				return err
			}
			return encodeOutput(cmd, map[string]any{"state": service.Store.Dir,
				"agents":  report.Agents,
				"runtime": report.Runtime, "runtimes": report.Runtimes, "untracked_processes": report.UntrackedProcesses,
				"sessions": report.Sessions, "notes": report.Notes})
		},
	}
}

func newPruneCommand(opts *commandOptions) *cobra.Command {
	var dryRun bool
	var selected []string
	cmd := &cobra.Command{
		Use: "prune", Short: "Retire stale registrations while preserving message history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			report, err := service.Store.Prune(dryRun, selected)
			if err != nil {
				return err
			}
			return encodeOutput(cmd, report)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview confirmed stale sessions and exited runtimes without changing records")
	cmd.Flags().StringArrayVar(&selected, "session", nil, "explicitly retire this exact kind:UUID, even if owner liveness is unknown; repeatable")
	return cmd
}
