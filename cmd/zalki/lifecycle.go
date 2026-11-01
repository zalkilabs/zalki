package main

import (
	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/install"
)

func newLifecycleCommand(opts *commandOptions, action string) *cobra.Command {
	var apply bool
	short, long := "Preview removal of managed hooks and MCP entries, preserving message history", ""
	if action == "upgrade" {
		short = "Preview updating managed configuration to this binary"
		long = short + ".\n\nAfter applying, " + upgradeSteps
	}
	cmd := &cobra.Command{
		Use: action, Short: short, Long: long, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			binary, err := installationBinary()
			if err != nil {
				return err
			}
			report, err := install.Manage(opts.registry.Configuration, install.Request{
				Action: action, Binary: binary,
				StateDir: service.Store.Dir, HomeDir: opts.home, Apply: apply,
			})
			if err != nil {
				return err
			}
			if action == "upgrade" {
				return encodeOutput(cmd, upgradeReport{Report: report, NextSteps: upgradeSteps})
			}
			return encodeOutput(cmd, report)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "apply the planned settings changes, keeping backups")
	return cmd
}

// Older builds sharing the state directory cannot read records a newer listener
// writes, and then fail registry-wide. Zalki never restarts clients itself.
const upgradeSteps = "reconnect the Zalki MCP server in every open Claude, Codex and Cursor session, " +
	"and in each Cursor conversation cancel any running cursor-listen task and arm one new task. " +
	"Every Zalki process sharing this state directory must run the same build. Message history is kept."

type upgradeReport struct {
	install.Report
	NextSteps string `json:"next_steps"`
}
