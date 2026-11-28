package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
)

func newListCommand(opts *commandOptions) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use: "list", Short: "List registered sessions", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			return runList(cmd, viewOf(service), asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, jsonFlagHelp)
	return cmd
}

func runList(cmd *cobra.Command, port diagnosticsPort, asJSON bool) error {
	sessions, skipped, err := port.ListWithWarnings()
	if err != nil {
		return failure("Zalki could not read the session records.", sessionRecordsNext, err)
	}
	if skipped > 0 {
		mark := markText(cmd.ErrOrStderr(), markNote)
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "%s %s\nNext: run zalki doctor to see why.\n", mark, skippedRecordsLine(skipped))
	}
	if err := noticeExitedSessions(cmd, port); err != nil {
		return err
	}
	if asJSON {
		return encodeOutput(cmd, sessions)
	}
	return printSessions(cmd, sessions)
}

// sessionRecordsNext is the one next step when the session records cannot be
// read at all.
const sessionRecordsNext = "run zalki doctor; the Zalki log has the details."

func skippedRecordsLine(n int) string {
	if n == 1 {
		return "1 session record cannot be read, so it is not listed."
	}
	return fmt.Sprintf("%d session records cannot be read, so they are not listed.", n)
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
	out := map[string]any{"state": port.Dir(),
		"agents":  report.Agents,
		"runtime": report.Runtime, "runtimes": report.Runtimes, "untracked_processes": report.UntrackedProcesses,
		"sessions": report.Sessions, "notes": report.Notes,
		"plugins_background": doctorBackground(cmd, opts, store)}
	adapterSections(opts, report, out)
	return encodeOutput(cmd, out)
}

func newPruneCommand(opts *commandOptions) *cobra.Command {
	var dryRun, asJSON bool
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
			return runPrune(cmd, viewOf(service), dryRun, selected, retention, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, jsonFlagHelp)
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "preview confirmed stale sessions, exited runtimes, and expired retired records without changing them")
	cmd.Flags().StringArrayVar(&selected, "session", nil, "explicitly retire this exact kind:UUID, even if owner liveness is unknown; repeatable")
	cmd.Flags().DurationVar(&retiredOlderThan, "retired-older-than", bus.DefaultRetiredRetention, "delete retired session records older than this duration; 0 deletes every retired record; message history is kept")
	return cmd
}

func runPrune(cmd *cobra.Command, port diagnosticsPort, dryRun bool, selected []string, retention time.Duration, asJSON bool) error {
	report, err := port.PruneRetired(dryRun, selected, retention)
	if err != nil {
		return failure("Zalki could not clean up the session records.", sessionRecordsNext, err)
	}
	if !dryRun {
		if err := noticeExitedSessions(cmd, port); err != nil {
			return err
		}
	}
	if asJSON {
		return encodeOutput(cmd, report)
	}
	return printPrune(cmd, report)
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
