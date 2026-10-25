package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/mcpserver"

	"github.com/spf13/cobra"
)

func newMCPCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "mcp", Short: "Serve the zalki MCP tools over stdio", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The SDK closes both streams to unblock IO on cancellation.
			input, inputOK := cmd.InOrStdin().(io.ReadCloser)
			output, outputOK := cmd.OutOrStdout().(io.WriteCloser)
			if !inputOK || !outputOK {
				return errors.New("MCP requires closable input and output streams")
			}
			service, err := opts.service()
			if err != nil {
				return err
			}
			lease, err := service.Store.TrackRuntime("mcp", "")
			if err != nil {
				return err
			}
			defer lease.Close() //nolint:errcheck // Stale runtime records can be pruned after shutdown.
			service.Runtime = lease
			opts.logger.InfoContext(cmd.Context(), "mcp.started", "runtime_id", lease.Info().ID)
			defer opts.logger.InfoContext(cmd.Context(), "mcp.stopped")
			return mcpserver.Run(cmd.Context(), service, input, output)
		},
	}
}

func newHookCommand(opts *commandOptions) *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use: "hook", Short: "Handle a native agent lifecycle event from JSON stdin", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			return service.Hook(kind, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&kind, "agent", "", "registered native agent (required)")
	if err := cmd.MarkFlagRequired("agent"); err != nil {
		panic(err)
	}
	if err := cmd.RegisterFlagCompletionFunc("agent", cobra.FixedCompletions(bus.AgentKinds(), cobra.ShellCompDirectiveNoFileComp)); err != nil {
		panic(err)
	}
	return cmd
}

func newCursorListenCommand(opts *commandOptions) *cobra.Command {
	var actor string
	cmd := &cobra.Command{
		Use: "cursor-listen", Short: "Run Cursor's native background-task inbox listener", Args: cobra.NoArgs,
		Long: "Run Cursor's inbox listener as a native Shell background task with output notifications.\nRequires your private SessionStart capability in ZALKI_CAPABILITY.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			key := os.Getenv("ZALKI_CAPABILITY")
			if key == "" {
				return errors.New("set ZALKI_CAPABILITY to your own private SessionStart capability for this command")
			}
			service, err := opts.service()
			if err != nil {
				return err
			}
			lease, err := service.Store.TrackRuntime("cursor_listener", actor)
			if err != nil {
				return err
			}
			defer lease.Close() //nolint:errcheck // Stale runtime records can be pruned after shutdown.
			return service.Store.ListenCursor(cmd.Context(), actor, key, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&actor, "actor", os.Getenv("ZALKI_ACTOR"), "your own Cursor actor ID (defaults to ZALKI_ACTOR)")
	return cmd
}

func newSendCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "send", Short: "Send a diagnostic agent message from JSON stdin", Args: cobra.NoArgs,
		Long: "Send through the same authenticated service as MCP. Read the message JSON from stdin\nand your private SessionStart capability from ZALKI_CAPABILITY.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Keep message text out of shell arguments and credentials out of receipts.
			var args bus.ToolArgs
			decoder := json.NewDecoder(io.LimitReader(cmd.InOrStdin(), 128*1024))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&args); err != nil {
				return err
			}
			if args.Capability != "" {
				return errors.New("provide your own capability through ZALKI_CAPABILITY, not message JSON")
			}
			key := os.Getenv("ZALKI_CAPABILITY")
			if key == "" {
				return errors.New("set ZALKI_CAPABILITY to your own private SessionStart capability for this command")
			}
			service, err := opts.service()
			if err != nil {
				return err
			}
			message, err := service.Send(cmd.Context(), bus.SendRequest{
				Actor: args.Actor, Capability: key,
				To: args.To, MessageID: args.MessageID, Body: args.Body,
			})
			if err != nil {
				return err
			}
			return encodeOutput(cmd, message)
		},
	}
}
