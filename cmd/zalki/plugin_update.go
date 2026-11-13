package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

var errPluginUpdateAvailable = errors.New("update available")

// Only the exact success marker means an available update. A joined cleanup
// error, or a build tool's exit status, remains an ordinary CLI failure.
func commandExitCode(err error) int {
	if !errors.Is(err, errPluginUpdateAvailable) {
		return 1
	}
	// Any wrapper can carry an independent cleanup failure. The successful
	// check path preserves its leaf marker when cleanup succeeds.
	var single interface{ Unwrap() error }
	var multiple interface{ Unwrap() []error }
	if errors.As(err, &single) || errors.As(err, &multiple) {
		return 1
	}
	return 2
}

func newPluginUpdateCommand(opts *commandOptions) *cobra.Command {
	var flags installFlags
	var check bool
	cmd := &cobra.Command{
		Use: "update NAME [FOLDER]", Short: "Review and replace a plugin while keeping its identity and data",
		Long: "Update uses the installed source folder unless FOLDER is given. It keeps identity, data, scope and enabled state. New access starts unselected; review the access changes in your own terminal. Stop a running plugin before updating. --yes accepts only unchanged standard access. --check reads state without changing it and exits 0 for up to date, 2 for an update, or 1 for an error.\n\n" + installWarning,
		Args: cobra.RangeArgs(1, 2),
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if check {
				opts.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
				return nil
			}
			return opts.root.PersistentPreRunE(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			var store bus.Store
			if check {
				if flags.yes || flags.tree != "" || flags.allowUnsandboxed || flags.details {
					return errors.New("--check cannot be combined with review or acceptance flags")
				}
				paths, err := opts.resolvedPaths()
				if err != nil {
					return err
				}
				store = bus.Store{Dir: paths.StateDir.Value}
			} else {
				service, err := opts.service()
				if err != nil {
					return err
				}
				store = service.Store
			}
			p := &pluginInstaller{cmd: cmd, store: store, flags: flags, home: opts.home, name: args[0]}
			folder := ""
			if len(args) == 2 {
				folder = args[1]
			}
			return p.runUpdate(folder, check)
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "check for a changed source snapshot without state writes or building")
	cmd.Flags().BoolVar(&flags.details, "details", false, "show copied source, pin, build and exact access details")
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "accept an update only with unchanged standard access")
	cmd.Flags().StringVar(&flags.tree, "tree-sha256", "", "expected snapshot hash (with --yes)")
	cmd.Flags().BoolVar(&flags.allowUnsandboxed, "allow-unsandboxed", false, "accept native code running as you (with --yes)")
	return cmd
}

func (p *pluginInstaller) runUpdate(folder string, check bool) (err error) {
	defer func() { err = pluginSourceError(err) }()
	if !p.flags.yes && (p.flags.tree != "" || p.flags.allowUnsandboxed) {
		return errors.New("--tree-sha256 and --allow-unsandboxed need --yes")
	}
	feed := pluginfeed.NewPluginFeed(p.store, p.name)
	status, err := p.loadUpdateBase(feed)
	if err != nil {
		return err
	}
	staging := ""
	if check {
		staging, err = os.MkdirTemp("", "zalki-update-check-")
		if err != nil {
			return err
		}
		defer func() {
			if cleanupErr := os.RemoveAll(staging); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}()
	} else {
		p.install, status, err = feed.BeginPluginUpdate()
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, p.install.Close()) }()
		staging = p.install.StagingDir()
	}
	p.update = &status
	if folder == "" {
		folder = status.Install.Source
	}
	if err := p.snapshotUpdate(folder, staging); err != nil {
		return err
	}
	if p.source.TreeSHA256 == status.Install.TreeSHA256 && p.folder == status.Install.Source {
		return p.say("%s is up to date (snapshot %s). Nothing was changed.\n", p.name, p.source.TreeSHA256)
	}
	if check {
		if err := p.say("%s: update available (snapshot %s -> %s). Run: zalki plugins update %s\n", p.name, status.Install.TreeSHA256, p.source.TreeSHA256, p.name); err != nil {
			return err
		}
		return errPluginUpdateAvailable
	}
	return p.applyUpdate()
}

func (p *pluginInstaller) snapshotUpdate(folder, staging string) error {
	first, err := p.readSource(folder)
	if err != nil {
		return fmt.Errorf("%w. If the folder moved, run: zalki plugins update %s NEW_FOLDER. Nothing was changed", err, p.name)
	}
	if first.ID != p.update.Install.ID {
		return fmt.Errorf("the folder holds plugin %q, not %q. Nothing was changed", first.ID, p.update.Install.ID)
	}
	if p.source, err = plugins.SnapshotInstallSource(p.folder, staging); err != nil {
		return err
	}
	if p.source.Manifest.ID != first.ID || p.source.Manifest.Name != first.Name || p.source.Manifest.Version != first.Version {
		return errors.New("the folder changed while it was copied; nothing was changed")
	}
	return nil
}

func (p *pluginInstaller) applyUpdate() (err error) {
	defer restoreSignalsOnCancel(p.cmd.Context())()
	p.installerActor, p.installerCapability = os.Getenv("ZALKI_ACTOR"), os.Getenv("ZALKI_CAPABILITY")
	if len(p.source.Manifest.Build) > 0 {
		p.tool, err = plugins.ResolveInstallTool(p.source.Manifest.Build[0])
		if err != nil {
			return err
		}
	}
	access, ok, err := p.review()
	if err == nil && ok {
		err = p.finish(access)
	} else if err == nil {
		err = errInstallCancelled
	}
	if cancelled(err) {
		return p.say("Cancelled. The old version is still installed.\n")
	}
	return err
}

func updateCommitError(name string) func(bool, error) error {
	return func(kept bool, err error) error {
		if err == nil {
			return nil
		}
		if kept {
			return fmt.Errorf("updating was not confirmed: %w. Check it with: zalki plugins show %s", err, name)
		}
		return fmt.Errorf("updating failed before publication: %w. No update was published", err)
	}
}

func (p *pluginInstaller) reportUpdated() error {
	status, err := pluginfeed.NewPluginFeed(p.store, p.name).PluginStatus()
	if err != nil {
		return fmt.Errorf("updated, but details could not be read; run: zalki plugins show %s", p.name)
	}
	start := "zalki plugins run " + p.name
	if !status.Enabled {
		start = "zalki plugins enable " + p.name + " && " + start
		a, declared := status.Access, p.source.Manifest.Access
		if !a.Status && !a.Receive && len(a.Send)+len(a.Read) == 0 && !declared.Send && !declared.Read {
			kind := "--receive"
			if declared.Status {
				kind = "--status"
			}
			start = "zalki plugins grant " + p.name + " " + kind + " && " + start
		}
	}
	return p.say("Updated %s: snapshot %s -> %s. It stays %s. Installer: %s. Start it with: %s\n", p.name, p.update.Install.TreeSHA256[:12], p.source.TreeSHA256[:12], enabledText(status.Enabled), pluginInstallerText(status.Install), start)
}

func enabledText(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func (p *pluginInstaller) dryRun(folder string) (err error) {
	defer func() { err = pluginSourceError(err) }()
	if p.flags.yes || p.flags.tree != "" || p.flags.allowUnsandboxed || p.flags.as != "" {
		return errors.New("--dry-run cannot be combined with --yes, --tree-sha256, --allow-unsandboxed or --as")
	}
	if _, err := p.readSource(folder); err != nil {
		return err
	}
	staging, err := os.MkdirTemp("", "zalki-install-dry-run-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(staging)) }()
	p.source, err = plugins.SnapshotInstallSource(p.folder, staging)
	if err != nil {
		return err
	}
	p.name = p.source.Manifest.Name
	if len(p.source.Manifest.Build) > 0 {
		p.tool, err = plugins.ResolveInstallTool(p.source.Manifest.Build[0])
		if err != nil {
			return err
		}
	}
	if err := p.say("Dry run for %s: nothing is installed.\n%s", p.name, p.header()); err != nil {
		return err
	}
	defer restoreSignalsOnCancel(p.cmd.Context())()
	if len(p.source.Manifest.Build) > 0 {
		log := filepath.Join(staging, "build.log")
		if err := plugins.RunInstallBuild(p.cmd.Context(), p.tool, p.source.Manifest.Build, p.source.Dir, log); err != nil {
			return errors.Join(fmt.Errorf("dry run failed: %w. The log was not kept", err), p.say("%s\n", plainLines(plugins.TailInstallLog(log, 20))))
		}
	}
	artifact, err := plugins.StagedInstallArtifact(p.source.Dir, p.source.Manifest.Artifact)
	if err != nil {
		return err
	}
	launch, err := pluginfeed.NewPluginLaunch(artifact, p.source.Manifest.Args)
	if err != nil {
		return err
	}
	return p.say("Dry run passed: regular executable (sha256 %s). Snapshot %s. Nothing was installed.\n", launch.SHA256, p.source.TreeSHA256)
}

func (p *pluginInstaller) loadUpdateBase(feed pluginfeed.PluginFeed) (pluginfeed.PluginFeedStatus, error) {
	status, err := feed.ReadPluginUpdateStatus()
	if err != nil {
		return status, err
	}
	if status.ID == "" {
		return status, fmt.Errorf("%s is not installed. Use: zalki plugins install FOLDER", p.name)
	}
	if status.Install == nil {
		return status, fmt.Errorf("%s was added with --exec, not installed from a folder. Remove it and install it from its folder. Nothing was changed", p.name)
	}
	return status, nil
}
