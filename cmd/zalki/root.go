package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/logging"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

type commandOptions struct {
	home     string
	state    string
	logLevel string
	logFile  string
	logger   *slog.Logger
	registry *agent.Registry
}

// A fresh command tree keeps flags and IO local to each invocation.
func newRootCommand(home string) *cobra.Command {
	opts := &commandOptions{home: home, registry: adapters.Builtin()}
	root := &cobra.Command{
		Use:           "zalki",
		Short:         "Local messaging between Claude Code, Codex, and Cursor CLI sessions",
		Version:       bus.BuildVersion(),
		SilenceErrors: true, // main reports each failure once on stderr.
		SilenceUsage:  true, // Runtime errors must not produce protocol output.
	}
	root.SetVersionTemplate("{{.Version}}\n")
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		return opts.initLogger(cmd)
	}
	root.PersistentFlags().StringVar(&opts.state, "state", filepath.Join(home, ".local", "state", "zalki"), "shared private registry directory")
	level := os.Getenv("ZALKI_LOG_LEVEL")
	if level == "" {
		level = "info"
	}
	root.PersistentFlags().StringVar(&opts.logLevel, "log-level", level, "diagnostic level: debug, info, warn, error, off (ZALKI_LOG_LEVEL)")
	root.PersistentFlags().StringVar(&opts.logFile, "log-file", os.Getenv("ZALKI_LOG_FILE"), "private JSON log file; default STATE/logs/zalki.jsonl, - for stderr (ZALKI_LOG_FILE)")
	root.AddCommand(
		newMCPCommand(opts), newHookCommand(opts), newCursorListenCommand(opts),
		newSendCommand(opts), newListCommand(opts), newDoctorCommand(opts),
		newPruneCommand(opts), newSetupCommand(opts), newInstallCommand(opts),
		newUpgradeCommand(opts), newUninstallCommand(opts),
		newPluginCommand(opts), newPluginNotifierCommand(opts),
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

// initLogger opens the diagnostic log selected by --log-file and --log-level,
// defaulting to a private file in the state directory.
func (opts *commandOptions) initLogger(cmd *cobra.Command) error {
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
				attrs = append(attrs, "error_code", model.ErrorCodeOf(err), "error_kind", logging.ErrorKind(err))
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
	store, err := bus.NewStore(directory, opts.registry, opts.logger)
	if err != nil {
		return nil, err
	}
	return &bus.Service{Store: store, PluginInbox: pluginfeed.AdmitPluginInbox}, nil
}

func encodeOutput(cmd *cobra.Command, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}
