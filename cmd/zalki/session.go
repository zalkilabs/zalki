package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters/cursor"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/mcpserver"

	"github.com/spf13/cobra"
)

// The composition root binds the core lease to the client-owned port.
var _ cursor.ListenerHandle = (*bus.Lease)(nil)

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
			// The observer shares the server's lifetime but never its failures:
			// it logs and skips, and shutdown waits for it only briefly.
			observeCtx, stopObserver := context.WithCancel(cmd.Context())
			observerDone := make(chan struct{})
			go func() {
				defer close(observerDone)
				service.ObserveExits(observeCtx, bus.DefaultSweepInterval)
			}()
			defer func() {
				stopObserver()
				select {
				case <-observerDone:
				case <-time.After(2 * time.Second):
				}
			}()
			return mcpStopError(cmd.Context(), mcpserver.Run(cmd.Context(), service, input, output))
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
			return service.Hook(cmd.Context(), kind, cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&kind, "agent", "", "registered native agent (required)")
	if err := cmd.MarkFlagRequired("agent"); err != nil {
		panic(err)
	}
	if err := cmd.RegisterFlagCompletionFunc("agent", cobra.FixedCompletions(opts.registry.Kinds(), cobra.ShellCompDirectiveNoFileComp)); err != nil {
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
			acquire := func(actor, key string) (cursor.ListenerHandle, error) {
				lease, err := service.Store.AcquireListenerLease(actor, key)
				if err != nil {
					return nil, err
				}
				return lease, nil
			}
			return cursor.Listen(cmd.Context(), acquire, bus.InspectEndpoint, actor, key, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&actor, "actor", os.Getenv("ZALKI_ACTOR"), "your own Cursor actor ID (defaults to ZALKI_ACTOR)")
	return cmd
}

// sendArgs preserves the diagnostic CLI input contract independently of MCP.
type sendArgs struct {
	Actor       string `json:"actor"`
	Capability  string `json:"capability"`
	To          string `json:"to"`
	MessageID   string `json:"message_id"`
	Body        string `json:"body"`
	History     bool   `json:"history,omitempty"`
	Sent        bool   `json:"sent,omitempty"`
	Preview     bool   `json:"preview,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	IncludeBody bool   `json:"include_body,omitempty"`
	Cursor      string `json:"cursor,omitempty"`
	Task        string `json:"task,omitempty"`
}

func newSendCommand(opts *commandOptions) *cobra.Command {
	return &cobra.Command{
		Use: "send", Short: "Send a diagnostic agent message from JSON stdin", Args: cobra.NoArgs,
		Long: "Send through the same authenticated service as MCP. Read the message JSON from stdin\nand your private SessionStart capability from ZALKI_CAPABILITY. The JSON to field accepts explicit plugin:UUID. Stored, not delivered; owner reading is a later command. Not acknowledged.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			// Keep message text out of shell arguments and credentials out of receipts.
			var args sendArgs
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
