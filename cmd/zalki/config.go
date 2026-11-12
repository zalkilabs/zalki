package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/config"
)

const codexQueueFallbackExposure = "Enabling queue fallback passes the full message envelope in process arguments, visible to other local users through ps and other local process inspection (normally up to 30 seconds)."

func (opts *commandOptions) configInputs() config.Inputs {
	flags := map[string]string{}
	for _, name := range []string{"config", "state", "log-level", "log-file"} {
		flag := opts.root.PersistentFlags().Lookup(name)
		if flag.Changed {
			flags[name] = flag.Value.String()
		}
	}
	// Absolute selections still work if the former cwd has been removed.
	cwd, _ := os.Getwd()
	return config.Inputs{Home: opts.home, Cwd: cwd, Flags: flags}
}

func (opts *commandOptions) loadConfig() error {
	if opts.snapshot != nil {
		return nil
	}
	snapshot, err := config.Load(opts.configInputs())
	if err != nil {
		return err
	}
	opts.snapshot = &snapshot
	return nil
}

func (opts *commandOptions) resolvedPaths() (config.Paths, error) {
	if opts.snapshot != nil {
		return opts.snapshot.Paths, nil
	}
	return config.ResolvePaths(opts.configInputs())
}

func newConfigCommand(opts *commandOptions) *cobra.Command {
	group := &cobra.Command{
		Use: "config", Short: "Inspect JSON settings and resolved paths",
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			opts.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			return nil
		},
	}
	group.AddCommand(&cobra.Command{
		Use: "path", Short: "Print the selected config filename", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := config.ResolvePaths(opts.configInputs())
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), paths.ConfigFile.Value)
			return err
		},
	}, &cobra.Command{
		Use: "show", Short: "Print resolved settings with their sources", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.loadConfig(); err != nil {
				return err
			}
			encoder := json.NewEncoder(cmd.OutOrStdout())
			encoder.SetIndent("", "  ")
			return encoder.Encode(configOutput(*opts.snapshot))
		},
	})
	return group
}

type configShowOutput struct {
	Paths struct {
		ConfigFile config.Value[string] `json:"config_file"`
		StateDir   config.Value[string] `json:"state_dir"`
	} `json:"paths"`
	FileExists bool `json:"file_exists"`
	Settings   struct {
		Codex struct {
			AllowQueueFallback    config.Value[bool] `json:"allow_queue_fallback"`
			QueueFallbackExposure string             `json:"queue_fallback_exposure"`
		} `json:"codex"`
		Messages struct {
			MaxBodyBytes config.Value[int] `json:"max_body_bytes"`
		} `json:"messages"`
		Logging struct {
			Level config.Value[string] `json:"level"`
			File  config.Value[string] `json:"file"`
		} `json:"logging"`
		Prune struct {
			RetiredOlderThan config.Value[string] `json:"retired_older_than"`
		} `json:"prune"`
	} `json:"settings"`
}

func configOutput(snapshot config.Snapshot) configShowOutput {
	var output configShowOutput
	output.Paths.ConfigFile = snapshot.Paths.ConfigFile
	output.Paths.StateDir = snapshot.Paths.StateDir
	output.FileExists = snapshot.FileExists
	output.Settings.Codex.AllowQueueFallback = snapshot.Settings.CodexAllowQueueFallback
	output.Settings.Codex.QueueFallbackExposure = codexQueueFallbackExposure
	output.Settings.Messages.MaxBodyBytes = snapshot.Settings.MaxBodyBytes
	output.Settings.Logging.Level = snapshot.Settings.LogLevel
	output.Settings.Logging.File = snapshot.Settings.LogFile
	retention := snapshot.Settings.RetiredOlderThan
	output.Settings.Prune.RetiredOlderThan = config.Value[string]{
		Value: retention.Value.String(), Source: retention.Source, Origin: retention.Origin,
	}
	return output
}
