package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/logging"

	"github.com/spf13/cobra"
)

type commandOptions struct {
	home     string
	state    string
	logLevel string
	logFile  string
	logger   *slog.Logger
}

// A fresh command tree keeps flags and IO local to each invocation.
func newRootCommand(home string) *cobra.Command {
	opts := &commandOptions{home: home}
	root := &cobra.Command{
		Use:           "zalki",
		Short:         "Local messaging between Claude Code, Codex, and Cursor CLI sessions",
		Version:       bus.BuildVersion(),
		SilenceErrors: true, // main reports each failure once on stderr.
		SilenceUsage:  true, // Runtime errors must not produce protocol output.
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("experimental-claude") {
			enabled, _ := cmd.Flags().GetBool("experimental-claude")
			if !enabled {
				return fmt.Errorf("Claude is enabled by default; remove the obsolete --experimental-claude flag")
			}
		}
		path := opts.logFile
		var stateRoot string
		if path == "" {
			path = filepath.Join(opts.state, "logs", "zalki.jsonl")
			var err error
			stateRoot, err = filepath.Abs(opts.state)
			if err != nil {
				return err
			}
		}
		if path != "-" {
			var err error
			path, err = filepath.Abs(path)
			if err != nil {
				return err
			}
		}
		logger, err := logging.New(logging.Config{Level: opts.logLevel, File: path, ExistingStateDir: stateRoot}, cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		opts.logger = logger.With("app", "zalki", "version", bus.BuildVersion(), "pid", os.Getpid(), "component", cmd.Name())
		return nil
	}
	root.PersistentFlags().StringVar(&opts.state, "state", filepath.Join(home, ".local", "state", "zalki"), "shared private registry directory")
	// Keep installed preview hooks working until upgrade rewrites their arguments.
	root.PersistentFlags().Bool("experimental-claude", false, "deprecated; Claude is enabled by default")
	_ = root.PersistentFlags().MarkHidden("experimental-claude")
	level := os.Getenv("ZALKI_LOG_LEVEL")
	if level == "" {
		level = "info"
	}
	root.PersistentFlags().StringVar(&opts.logLevel, "log-level", level, "diagnostic level: debug, info, warn, error, off (ZALKI_LOG_LEVEL)")
	root.PersistentFlags().StringVar(&opts.logFile, "log-file", os.Getenv("ZALKI_LOG_FILE"), "private JSON log file; default STATE/logs/zalki.jsonl, - for stderr (ZALKI_LOG_FILE)")
	root.AddCommand(
		newMCPCommand(opts), newHookCommand(opts), newCursorListenCommand(opts),
		newSendCommand(opts), newListCommand(opts), newDoctorCommand(opts),
		newPruneCommand(opts), newSetupCommand(opts, false), newSetupCommand(opts, true),
		newLifecycleCommand(opts, "upgrade"), newLifecycleCommand(opts, "uninstall"),
		&cobra.Command{
			Use: "version", Short: "Print the zalki build version", Args: cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), bus.BuildVersion())
				return err
			},
		},
	)
	observeCommands(root, opts)
	return root
}

func observeCommands(root *cobra.Command, opts *commandOptions) {
	for _, cmd := range root.Commands() {
		observeCommands(cmd, opts)
		if cmd.RunE == nil {
			continue
		}
		run := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			started := time.Now()
			opts.logger.DebugContext(cmd.Context(), "command.started")
			err := run(cmd, args)
			attrs := []any{"duration_ms", time.Since(started).Milliseconds()}
			if err != nil {
				attrs = append(attrs, "error_code", bus.ErrorCodeOf(err), "error_kind", logging.ErrorKind(err))
				opts.logger.ErrorContext(cmd.Context(), "command.failed", attrs...)
			} else {
				level := slog.LevelDebug
				apply, _ := cmd.Flags().GetBool("apply")
				if apply {
					level = slog.LevelInfo
				}
				opts.logger.Log(cmd.Context(), level, "command.completed", attrs...)
			}
			return err
		}
	}
}

func (opts *commandOptions) service() (*bus.Service, error) {
	directory, err := filepath.Abs(opts.state)
	if err != nil {
		return nil, err
	}
	return &bus.Service{Store: bus.Store{Dir: directory, Logger: opts.logger}}, nil
}

func encodeOutput(cmd *cobra.Command, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}
