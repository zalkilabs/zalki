package main

import (
	"io"
	"log/slog"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
)

func newPluginReadCommand(opts *commandOptions) *cobra.Command {
	var session, message string
	cmd := &cobra.Command{
		Use: "read NAME --session SESSION --message UUID", Short: "Read one explicitly granted inbox message without acknowledging it",
		Long: "Read one exact-ID inbox message through the installation's read grant. The plugin executable runs as the owner; grants filter only what Zalki exposes. Native pending bodies stay deferred. This owner proxy verifies the installation snapshot; it does not start a plugin or prove process liveness.",
		Args: cobra.ExactArgs(1),
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			opts.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			raw, err := bus.NewPluginFeed(service.Store, args[0]).ReadPluginMessage(session, message)
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
	cmd.Flags().StringVar(&session, "session", "", "exact granted canonical native session ID")
	cmd.Flags().StringVar(&message, "message", "", "exact message UUID; no listing or sent-history query")
	for _, flag := range []string{"session", "message"} {
		if err := cmd.MarkFlagRequired(flag); err != nil {
			panic(err)
		}
	}
	return cmd
}
