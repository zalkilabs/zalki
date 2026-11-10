package main

import (
	"io"
	"log/slog"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
)

func newPluginInboxReadCommand(opts *commandOptions) *cobra.Command {
	var message string
	cmd := &cobra.Command{
		Use: "inbox-read NAME --message UUID", Short: "Read one stored plugin inbox message without acknowledging it",
		Long: pluginTrustNotice + "\n\nOwner proxy: requires an explicit --inbox-read grant and a current checked installation; does not start a plugin. Exact message UUID only; no listing. Bodies are bounded and audited before output.", Args: cobra.ExactArgs(1),
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			opts.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			raw, err := bus.NewPluginFeed(service.Store, args[0]).ReadPluginInboxMessage(message)
			if err != nil {
				return err
			}
			n, err := cmd.OutOrStdout().Write(raw)
			if err == nil && n != len(raw) {
				return io.ErrShortWrite
			}
			return err
		},
	}
	cmd.Flags().StringVar(&message, "message", "", "exact stored plugin inbox message UUID")
	if err := cmd.MarkFlagRequired("message"); err != nil {
		panic(err)
	}
	return cmd
}
