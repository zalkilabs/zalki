package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
)

// printPrune is the words form of zalki prune. A preview says it changed
// nothing and how to apply; a run says what it did.
func printPrune(cmd *cobra.Command, report bus.PruneReport) error {
	out := cmd.OutOrStdout()
	retired, deleted := len(report.Sessions), len(report.DeletedSessions)
	files := len(report.Runtimes) + len(report.DeletedLocks) + len(report.DeletedFlood) + len(report.DeletedWatches)
	var parts []string
	if report.DryRun {
		parts = pruneParts("retire", "delete", "remove", retired, deleted, files)
	} else {
		parts = pruneParts("retired", "deleted", "removed", retired, deleted, files)
	}
	switch {
	case len(parts) == 0:
		_, err := fmt.Fprintln(out, "Nothing to clean up.")
		return err
	case report.DryRun:
		_, err := fmt.Fprintf(out, "Preview only, nothing changed. It would %s.\nNext: to apply, run the same command without --dry-run.\n", joinParts(parts))
		return err
	}
	_, err := fmt.Fprintf(out, "Done: %s. Message history was kept.\n", joinParts(parts))
	return err
}

// pruneParts names each non-zero kind of cleanup, in verb forms the caller picks.
func pruneParts(retire, del, remove string, retired, deleted, files int) []string {
	var parts []string
	if retired > 0 {
		parts = append(parts, fmt.Sprintf("%s %s", retire, countOf(retired, "stale session", "stale sessions")))
	}
	if deleted > 0 {
		parts = append(parts, fmt.Sprintf("%s %s", del, countOf(deleted, "old retired record", "old retired records")))
	}
	if files > 0 {
		parts = append(parts, fmt.Sprintf("%s %s", remove, countOf(files, "leftover file", "leftover files")))
	}
	return parts
}
