package main

import (
	"errors"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/plugins"
)

const pluginTrustNotice = "External plugins run with your OS user permissions. Session grants filter the Zalki feed; they do not restrict filesystem access. A plugin can read and change all of your Zalki state. Run only code you trust."

func newPluginCommand(opts *commandOptions) *cobra.Command {
	group := &cobra.Command{Use: "plugins", Short: "Manage explicitly enabled local plugins", Long: pluginTrustNotice}
	for _, name := range []string{"add", "remove", "configure", "enable", "disable", "status"} {
		group.AddCommand(newPluginOwnerCommand(opts, name))
	}
	group.AddCommand(newPluginCapabilityCommand(opts, "grant"), newPluginCapabilityCommand(opts, "revoke"))
	group.AddCommand(newPluginReadCommand(opts))
	group.AddCommand(newPluginCheckCommand(opts))
	group.AddCommand(&cobra.Command{
		Use: "run NAME", Short: "Run one foreground local notifier host", Long: pluginTrustNotice + "\n\nPlugins MUST exit on stdin EOF within the 2-second grace period and must not daemonize. Helpers must exit with the plugin or not inherit FD 3. Interactive terminal input is unsupported.", Args: cobra.ExactArgs(1),
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
