package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func newPluginOwnerCommand(opts *commandOptions, action string) *cobra.Command {
	var scope string
	var executable string
	cmd := &cobra.Command{
		Long: pluginTrustNotice,
		Use:  action + " NAME", Short: "Manage a local plugin's grants and enabled state", Args: func(cmd *cobra.Command, args []string) error {
			if action == "add" {
				if len(args) > 1 && cmd.ArgsLenAtDash() != 1 {
					return errors.New("plugin arguments must follow --")
				}
				return cobra.MinimumNArgs(1)(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPluginOwner(cmd, opts, action, args, scope, executable)
		},
	}
	if action == "configure" {
		cmd.Flags().StringVar(&scope, "scope", "", "all: standard access (status, receive) applies to every live session; none: to no session")
	}
	if action == "add" {
		cmd.Flags().StringVar(&executable, "exec", "", "absolute executable; arguments after -- are literal and visible in process listings")
	}
	return cmd
}

// pluginDone prints one plain line after a successful owner command.
func pluginDone(cmd *cobra.Command, err error, format string, args ...any) error {
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), format+"\n", args...)
	return err
}

func printPluginStatus(cmd *cobra.Command, store bus.Store, name string, target *loginItemTarget) error {
	status, err := pluginfeed.NewPluginFeed(store, name).PluginStatus()
	if err != nil {
		return err
	}
	run, _, _ := readRunView(store.Dir, name, status, time.Now())
	background, _ := backgroundView(cmd.Context(), target)
	value := struct {
		pluginfeed.PluginFeedStatus
		PinMatches *bool                `json:"pin_matches,omitempty"`
		PinError   string               `json:"pin_error,omitempty"`
		Run        pluginRunView        `json:"run"`
		Background pluginBackgroundView `json:"background"`
	}{PluginFeedStatus: status, Run: run, Background: background}
	if status.Launch != nil {
		err := status.Launch.Verify()
		ok := err == nil
		value.PinMatches = &ok
		if err != nil {
			value.PinError = err.Error()
		}
	}
	return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
}

func runPluginOwner(cmd *cobra.Command, opts *commandOptions, action string, args []string, scope, executable string) error {
	if action == "configure" && scope == "all" && !pluginAccessTerminal(cmd.InOrStdin()) {
		return errors.New("change plugin scope in your own terminal: zalki plugins configure NAME --scope all")
	}
	service, err := opts.service()
	if err != nil {
		return err
	}
	feed, name := pluginfeed.NewPluginFeed(service.Store, args[0]), plainText(args[0])
	switch action {
	case "add":
		if executable == "" {
			return errors.New("plugins add requires --exec ABSOLUTE_PATH; or use zalki plugins install FOLDER")
		}
		return pluginDone(cmd, feed.AddExternalPlugin(executable, args[1:]), "Added %s (disabled).", name)
	case "remove":
		if err := removePluginOwner(cmd, feed, name); err != nil {
			return err
		}
		if target := newLoginItemTarget(opts, service.Store.Dir); target != nil {
			if line := target.idleLine(cmd.Context(), service.Store); line != "" {
				return sayLine(cmd, line)
			}
		}
		return nil
	case "configure":
		return configurePluginOwner(cmd, feed, name, scope)
	case "enable":
		return enablePluginOwner(cmd, feed, service.Store.Dir, args[0], name, newLoginItemTarget(opts, service.Store.Dir))
	case "disable":
		return pluginDone(cmd, feed.EnablePluginFeed(false), "Disabled %s.", name)
	default:
		return printPluginStatus(cmd, service.Store, args[0], newLoginItemTarget(opts, service.Store.Dir))
	}
}

func removePluginOwner(cmd *cobra.Command, feed pluginfeed.PluginFeed, name string) error {
	before, err := feed.PluginStatus()
	if err != nil && !errors.Is(err, pluginfeed.ErrOldPluginRecord) {
		return err
	}
	err = feed.RemovePlugin()
	if before.Enabled {
		return pluginDone(cmd, err, "Disabled and removed %s.", name)
	}
	return pluginDone(cmd, err, "Removed %s.", name)
}

func configurePluginOwner(cmd *cobra.Command, feed pluginfeed.PluginFeed, name, scope string) error {
	before, err := feed.PluginStatus()
	if err != nil {
		return err
	}
	err = feed.ConfigurePluginScope(scope)
	if before.Enabled {
		return pluginAccessRecorded(cmd, err, name)
	}
	return pluginDone(cmd, err, "Configured %s: scope %s.", name, plainText(scope))
}

// Owner lines of a direct enable.
const (
	enabledLine             = "Enabled %s."
	enabledResetUnconfirmed = "Enabled %s. Clearing its crash limit could not be confirmed. Run: zalki plugins restart %s"
)

// enablePluginOwner enables the plugin. A confirmed disabled->enabled change
// of the same installation also writes a restart request, which clears its
// crash limit, as zalki plugins restart does. An enable that was already
// true writes none. A failed request after a good enable is a partial
// success (exit 1): the request may even be on disk, so the line says only
// that clearing could not be confirmed.
func enablePluginOwner(cmd *cobra.Command, feed pluginfeed.PluginFeed, state, raw, name string, target *loginItemTarget) error {
	before, err := feed.PluginStatus()
	if err != nil {
		return pluginDone(cmd, feed.EnablePluginFeed(true), enabledLine, name)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		return err
	}
	if before.Enabled {
		return pluginDone(cmd, nil, enabledLine, name)
	}
	after, err := feed.PluginStatus()
	if err == nil && (!after.Enabled || after.ID == "" || after.ID != before.ID) {
		err = errors.New("the enabled installation was not confirmed")
	}
	if err == nil {
		err = requestRestart(state, raw, after, time.Now())
	}
	if err != nil {
		return fmt.Errorf(enabledResetUnconfirmed, name, name)
	}
	// A confirmed change: an idle supervisor has exited, so wake it.
	// Without a login item (a custom state) the line stays as it was.
	if result := wakeSupervisor(cmd.Context(), target, state); result != wakeManual {
		return pluginDone(cmd, nil, enabledLine+"\n%s", name, wakeEnableLine(result, name))
	}
	return pluginDone(cmd, nil, enabledLine, name)
}
