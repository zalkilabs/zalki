package main

import (
	"errors"
	"fmt"

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
			sessions, skipped, err := service.Store.ListWithWarnings()
			if err != nil {
				return err
			}
			if skipped > 0 {
				_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: skipped %d unreadable or unsupported session records.\n", skipped)
			}
			if err := noticeExitedSessions(cmd, service); err != nil {
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
			report, err := service.Store.Diagnostics(cmd.Context())
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
			if !dryRun {
				if err := noticeExitedSessions(cmd, service); err != nil {
					return err
				}
			}
			return encodeOutput(cmd, report)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview confirmed stale sessions, exited runtimes, and expired retired records without changing them")
	cmd.Flags().StringArrayVar(&selected, "session", nil, "explicitly retire this exact kind:UUID, even if owner liveness is unknown; repeatable")
	cmd.Flags().DurationVar(&retiredOlderThan, "retired-older-than", bus.DefaultRetiredRetention, "delete retired session records older than this duration; 0 deletes every retired record; message history is kept")
	return cmd
}

// Notice delivery is best effort; journal observation failures remain fatal.
func noticeExitedSessions(cmd *cobra.Command, service *bus.Service) error {
	err := service.NoticeExitedSessions(cmd.Context())
	var delivery *bus.NoticeDeliveryError
	if !errors.As(err, &delivery) {
		return err
	}
	// The receipt retains details; do not echo native errors or message text.
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Warning: a completion notice could not be delivered; see its receipt.")
	return nil
}
