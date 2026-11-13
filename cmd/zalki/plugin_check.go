package main

import (
	"errors"
	"io"
	"log/slog"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

func newPluginCheckCommand(opts *commandOptions) *cobra.Command {
	var executable string
	cmd := &cobra.Command{
		Use: "check [NAME] [--exec ABS -- ARGS...]", Short: "Check observed plugin transport in temporary HOME and cwd",
		Long: pluginTrustNotice + "\n\nThe checker uses temporary HOME and cwd paths. These paths do not sandbox the executable or restrict access to real files.",
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			opts.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			return nil
		},
		Args: func(cmd *cobra.Command, args []string) error {
			if executable == "" {
				return cobra.ExactArgs(1)(cmd, args)
			}
			if len(args) > 0 && cmd.ArgsLenAtDash() != 0 {
				return errors.New("use NAME or --exec; executable arguments must follow --")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if executable != "" {
				launch, err := pluginfeed.NewPluginLaunch(executable, args)
				if err != nil {
					return err
				}
				return plugins.Check(cmd.Context(), plugins.CheckTarget{Launch: &launch}, cmd.OutOrStdout())
			}
			service, err := opts.service()
			if err != nil {
				return err
			}
			target, closeLease, err := namedCheckTarget(service.Store, args[0])
			if err != nil {
				return err
			}
			defer closeLease()
			return plugins.Check(cmd.Context(), target, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&executable, "exec", "", "absolute executable; owner arguments after -- are literal")
	return cmd
}

func namedCheckTarget(store bus.Store, name string) (plugins.CheckTarget, func(), error) {
	feed := pluginfeed.NewPluginFeed(store, name)
	status, err := feed.PluginStatus()
	if err != nil {
		return plugins.CheckTarget{}, nil, err
	}
	if status.ID == "" {
		return plugins.CheckTarget{}, nil, errors.New("plugin is not installed")
	}
	lease, err := feed.AcquirePluginLease()
	if err != nil {
		return plugins.CheckTarget{}, nil, err
	}
	closeLease := func() { _ = lease.Close() }
	current, err := feed.PluginStatus()
	if err != nil || current.ID != status.ID {
		closeLease()
		return plugins.CheckTarget{}, nil, errors.Join(err, errors.New("plugin installation changed before check"))
	}
	if current.Launch == nil {
		closeLease()
		return plugins.CheckTarget{}, nil, errors.New("plugin has no external executable")
	}
	return plugins.CheckTarget{Launch: current.Launch, Lease: lease}, closeLease, nil
}
