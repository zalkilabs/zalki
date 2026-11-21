package main

import (
	"errors"
	"fmt"
	"time"

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
			return runList(cmd, viewOf(service))
		},
	}
}

func runList(cmd *cobra.Command, port diagnosticsPort) error {
	sessions, skipped, err := port.ListWithWarnings()
	if err != nil {
		return err
	}
	if skipped > 0 {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: skipped %d unreadable or unsupported session records; run zalki doctor.\n", skipped)
	}
	if err := noticeExitedSessions(cmd, port); err != nil {
		return err
	}
	return encodeOutput(cmd, sessions)
}

func newDoctorCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "doctor", Short: "Report builds, processes, and receive health as JSON", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			return runDoctor(cmd, opts, viewOf(service), service.Store)
		},
	}
}

// The plugin section reads the plugin records, so it takes the store itself.
func runDoctor(cmd *cobra.Command, opts *commandOptions, port diagnosticsPort, store bus.Store) error {
	report, err := port.Diagnostics(cmd.Context())
	if err != nil {
		return err
	}
	return encodeOutput(cmd, map[string]any{"state": port.Dir(),
		"agents":  report.Agents,
		"runtime": report.Runtime, "runtimes": report.Runtimes, "untracked_processes": report.UntrackedProcesses,
		"sessions": report.Sessions, "notes": report.Notes,
		"codex_hooks":        diagnoseCodexHooks(opts, report),
		"plugins_background": doctorBackground(cmd, opts, store)})
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
			retention := opts.snapshot.Settings.RetiredOlderThan.Value
			if cmd.Flags().Changed("retired-older-than") {
				retention = retiredOlderThan
			}
			return runPrune(cmd, viewOf(service), dryRun, selected, retention)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview confirmed stale sessions, exited runtimes, and expired retired records without changing them")
	cmd.Flags().StringArrayVar(&selected, "session", nil, "explicitly retire this exact kind:UUID, even if owner liveness is unknown; repeatable")
	cmd.Flags().DurationVar(&retiredOlderThan, "retired-older-than", bus.DefaultRetiredRetention, "delete retired session records older than this duration; 0 deletes every retired record; message history is kept")
	return cmd
}

func runPrune(cmd *cobra.Command, port diagnosticsPort, dryRun bool, selected []string, retention time.Duration) error {
	report, err := port.PruneRetired(dryRun, selected, retention)
	if err != nil {
		return err
	}
	if !dryRun {
		if err := noticeExitedSessions(cmd, port); err != nil {
			return err
		}
	}
	return encodeOutput(cmd, report)
}

// Notice delivery is best effort; journal observation failures remain fatal.
func noticeExitedSessions(cmd *cobra.Command, port diagnosticsPort) error {
	err := port.NoticeExitedSessions(cmd.Context())
	var delivery *bus.NoticeDeliveryError
	if !errors.As(err, &delivery) {
		return err
	}
	// The receipt retains details; do not echo native errors or message text.
	_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "Warning: a completion notice could not be delivered; see its receipt.")
	return nil
}
