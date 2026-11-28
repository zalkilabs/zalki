package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// printDoctor is the words form of zalki doctor: one line per finding with a
// mark, the fix under it, and one summary line.
func printDoctor(cmd *cobra.Command, checks []doctorCheck, summary doctorSummary) error {
	out := cmd.OutOrStdout()
	const markWidth = len("[problem]")
	for _, c := range checks {
		mark := markText(out, c.Status)
		pad := strings.Repeat(" ", markWidth-len(c.Status)-2)
		if _, err := fmt.Fprintf(out, "%s%s %s\n", mark, pad, c.Name); err != nil {
			return err
		}
		if c.Fix != "" {
			if _, err := fmt.Fprintf(out, "%s Next: %s\n", strings.Repeat(" ", markWidth), c.Fix); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprintf(out, "\n%s\n", summaryLine(summary))
	return err
}

// summaryLine says how many findings there were and, with a problem, what to do.
func summaryLine(s doctorSummary) string {
	parts := []string{fmt.Sprintf("%d ok", s.OK)}
	if s.Notes > 0 {
		parts = append(parts, countOf(s.Notes, "note", "notes"))
	}
	if s.Problems > 0 {
		parts = append(parts, countOf(s.Problems, "problem", "problems"))
		return "Doctor: " + joinParts(parts) + ".\nNext: fix the problems marked above, then run zalki doctor again."
	}
	return "Doctor: " + joinParts(parts) + ". No problems found."
}
