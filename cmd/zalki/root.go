// Package main provides the zalki CLI and composes native adapters, the core
// message journal, plugin state and plugin process hosts. It owns install
// prompts and orchestration; plugin build and commit live in internal packages.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/config"
	"github.com/zalkilabs/zalki/internal/install"
	"github.com/zalkilabs/zalki/internal/logging"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

type commandOptions struct {
	root       *cobra.Command
	home       string
	state      string
	configFile string
	logLevel   string
	logFile    string
	logger     *slog.Logger
	registry   *agent.Registry
	snapshot   *config.Snapshot
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
	opts.root = root
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if err := opts.loadConfig(); err != nil && cmd.Name() != "version" {
			return err
		}
		return opts.initLogger(cmd)
	}
	root.PersistentFlags().StringVar(&opts.configFile, "config", "", "JSON settings file (ZALKI_CONFIG; default XDG_CONFIG_HOME/zalki/config.json or ~/.config/zalki/config.json)")
	root.PersistentFlags().StringVar(&opts.state, "state", "", "shared private registry directory (ZALKI_STATE; default XDG_STATE_HOME/zalki or ~/.local/state/zalki)")
	level := os.Getenv("ZALKI_LOG_LEVEL")
	if level == "" {
		level = "info"
	}
	root.PersistentFlags().StringVar(&opts.logLevel, "log-level", level, "diagnostic level: debug, info, warn, error, off (ZALKI_LOG_LEVEL)")
	root.PersistentFlags().StringVar(&opts.logFile, "log-file", os.Getenv("ZALKI_LOG_FILE"), "private JSON log file; default STATE/logs/zalki.jsonl, - for stderr (ZALKI_LOG_FILE)")
	root.AddCommand(
		newConfigCommand(opts),
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
	paths, err := opts.resolvedPaths()
	if err != nil {
		return err
	}
	cfg, err := opts.loggerConfig(paths)
	if err != nil {
		return err
	}
	declare := installDeclarations(opts.registry, paths.ConfigFile.Value)
	if cmd.Name() == "uninstall" {
		declare = nil
	}
	if err := install.CheckLogFile(declare, cfg.File, paths.StateDir.Value, opts.home); err != nil {
		return err
	}
	logger, err := logging.New(cfg, cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	opts.logger = logger.With("app", "zalki", "version", bus.BuildVersion(), "pid", os.Getpid(), "component", cmd.Name())
	return nil
}

func (opts *commandOptions) loggerConfig(paths config.Paths) (logging.Config, error) {
	level, path := opts.logLevel, opts.logFile
	fromState := path == ""
	if opts.snapshot != nil {
		level = opts.snapshot.Settings.LogLevel.Value
		path = opts.snapshot.Settings.LogFile.Value
		fromState = opts.snapshot.Settings.LogFileFromState
	}
	var stateRoot string
	if fromState {
		path = paths.DefaultLogFile
		stateRoot = paths.StateDir.Value
	}
	path, err := absoluteLogPath(path)
	if err != nil {
		return logging.Config{}, err
	}
	if err := checkLogConfigFile(path, paths.ConfigFile.Value); err != nil {
		return logging.Config{}, err
	}
	return logging.Config{Level: level, File: path, ExistingStateDir: stateRoot}, nil
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
	if err := opts.loadConfig(); err != nil {
		return nil, err
	}
	store, err := bus.NewStore(opts.snapshot.Paths.StateDir.Value, opts.registry, opts.logger)
	if err != nil {
		return nil, err
	}
	store.MaxBodyBytes = opts.snapshot.Settings.MaxBodyBytes.Value
	store.ConfigFile = opts.snapshot.Paths.ConfigFile.Value
	return &bus.Service{Store: store, PluginInbox: pluginfeed.AdmitPluginInbox}, nil
}

func encodeOutput(cmd *cobra.Command, value any) error {
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}
