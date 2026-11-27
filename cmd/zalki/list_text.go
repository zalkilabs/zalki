package main

import (
	"fmt"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/model"
)

const (
	listTaskWidth = 60
	noSessionsMsg = "No sessions are registered."
	noSessionsFix = "start a client with Zalki set up, then run zalki list again; zalki doctor checks the setup."
)

// printSessions is the words form of zalki list: one line per session with the
// address to send to, its name, what it is doing now and its own task claim.
func printSessions(cmd *cobra.Command, sessions []model.Session) error {
	out := cmd.OutOrStdout()
	if len(sessions) == 0 {
		_, err := fmt.Fprintf(out, "%s\nNext: %s\n", noSessionsMsg, noSessionsFix)
		return err
	}
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(table, "SESSION\tNAME\tSTATE\tTASK")
	for _, s := range sessions {
		_, _ = fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", plainText(s.ID), orDash(plainText(s.Name)), sessionStateWord(s), orDash(shorten(plainText(s.Task), listTaskWidth)))
	}
	return table.Flush()
}

// sessionStateWord is one plain word for what a session is doing now.
func sessionStateWord(s model.Session) string {
	switch {
	case s.Retired || s.State == model.StateRetired:
		return "retired"
	case s.Liveness == model.LivenessExited:
		return "exited"
	case s.State == model.StateApproval:
		return "needs approval"
	case s.State == model.StateBusy:
		return "busy"
	}
	return "ready"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shorten cuts s to at most n characters and marks the cut with "...".
func shorten(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return string(runes[:n-3]) + "..."
}
