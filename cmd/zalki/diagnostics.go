package main

import (
	"errors"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
)

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
	retiredOlderThan := bus.DefaultRetiredRetention
	cmd := &cobra.Command{
		Use: "prune", Short: "Retire stale registrations and delete old retired records, keeping message history", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if retiredOlderThan < 0 {
				return errors.New("retired retention window must not be negative")
			}
			service, err := opts.service()
			if err != nil {
				return err
			}
			report, err := service.Store.PruneRetired(dryRun, selected, retiredOlderThan)
			if err != nil {
				return err
			}
			return encodeOutput(cmd, report)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview confirmed stale sessions, exited runtimes, and expired retired records without changing them")
	cmd.Flags().StringArrayVar(&selected, "session", nil, "explicitly retire this exact kind:UUID, even if owner liveness is unknown; repeatable")
	cmd.Flags().DurationVar(&retiredOlderThan, "retired-older-than", bus.DefaultRetiredRetention, "delete retired session records older than this duration; 0 deletes every retired record; message history is kept")
	return cmd
}
