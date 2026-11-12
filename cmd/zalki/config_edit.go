package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/config"
)

func newConfigEditCommand(opts *commandOptions, unset bool) *cobra.Command {
	verb, usage, arity := "set", "set KEY VALUE", 2
	if unset {
		verb, usage, arity = "unset", "unset KEY", 1
	}
	return &cobra.Command{
		Use: usage, Short: verb + " one private JSON setting; print its effective value and source",
		Long: "Edit one supported JSON setting atomically. String values are literal shell arguments. " +
			"Boolean and integer values use JSON syntax. Existing client commands keep their pinned --config path. " +
			"Value changes need no reinstall; restart or reconnect running clients to load a new snapshot. " +
			"After selecting another config path, refresh client commands with upgrade --config-only.",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != arity {
				return fmt.Errorf("use: zalki config %s", usage)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var value *string
			if !unset {
				value = &args[1]
			}
			snapshot, err := config.Update(opts.configInputs(), args[0], value)
			if err != nil {
				return err
			}
			return encodeOutput(cmd, configEditOutput(snapshot, args[0]))
		},
	}
}

type configEditResult struct {
	Key      string `json:"key"`
	Value    any    `json:"value"`
	Source   string `json:"source"`
	Origin   string `json:"origin"`
	Exposure string `json:"exposure,omitempty"`
}

func configEditOutput(snapshot config.Snapshot, key string) configEditResult {
	output := configOutput(snapshot)
	result := configEditResult{Key: key}
	switch key {
	case "codex.allow_queue_fallback":
		result = editResult(key, output.Settings.Codex.AllowQueueFallback)
		result.Exposure = codexQueueFallbackExposure
	case "messages.max_body_bytes":
		result = editResult(key, output.Settings.Messages.MaxBodyBytes)
	case "logging.level":
		result = editResult(key, output.Settings.Logging.Level)
	case "logging.file":
		result = editResult(key, output.Settings.Logging.File)
	case "prune.retired_older_than":
		result = editResult(key, output.Settings.Prune.RetiredOlderThan)
	}
	return result
}

func editResult[T any](key string, value config.Value[T]) configEditResult {
	return configEditResult{Key: key, Value: value.Value, Source: value.Source, Origin: value.Origin}
}
