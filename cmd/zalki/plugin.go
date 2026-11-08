package main

import (
	"errors"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/plugins"
)

func newPluginCommand(opts *commandOptions) *cobra.Command {
	group := &cobra.Command{Use: "plugins", Short: "Manage explicitly enabled local plugins"}
	for _, name := range []string{"add", "remove", "configure", "enable", "disable", "status"} {
		group.AddCommand(newPluginOwnerCommand(opts, name))
	}
	group.AddCommand(newPluginCheckCommand(opts))
	group.AddCommand(&cobra.Command{
		Use: "run NAME", Short: "Run one foreground local notifier host", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			binary, err := os.Executable()
			if err != nil {
				return err
			}
			return plugins.Run(cmd.Context(), service.Store, binary, args[0], cmd.OutOrStdout())
		},
	})
	return group
}

func newPluginNotifierCommand(opts *commandOptions) *cobra.Command {
	var outbox string
	cmd := &cobra.Command{
		Use: plugins.ChildCommand, Hidden: true, Args: cobra.NoArgs,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			// The child touches only its outbox, including when its parent dies.
			opts.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			return nil
		},
		RunE: func(cmd *cobra.Command, _ []string) error {
			input, ok := cmd.InOrStdin().(io.ReadCloser)
			if !ok {
				return errors.New("notifier requires closable stdin")
			}
			return plugins.RunNotifier(cmd.Context(), input, cmd.OutOrStdout(), outbox)
		},
	}
	cmd.Flags().StringVar(&outbox, "outbox", "", "private local outbox supplied by the host")
	if err := cmd.MarkFlagRequired("outbox"); err != nil {
		panic(err)
	}
	return cmd
}
