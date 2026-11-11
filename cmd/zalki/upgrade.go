package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/install"
)

const (
	caskName        = "zalkilabs/tap/zalki"
	upgradeSuccess  = "Upgrade successful"
	configOnlyDone  = "Configuration refreshed"
	brewUpgradeHint = "brew upgrade --cask " + caskName
	// brewPipeWaitDelay bounds inherited output-pipe waits after brew exits
	// or its context is cancelled.
	brewPipeWaitDelay = 2 * time.Second
)

// binaryLocation describes how the running binary was installed. Resolved is the
// real executable; Binary is the stable path clients are configured with.
type binaryLocation struct {
	Binary, Resolved string
}

// Tests replace these to run against disposable fixtures.
var (
	locateInstallation = func() (binaryLocation, error) {
		binary, err := installationBinary()
		if err != nil {
			return binaryLocation{}, err
		}
		executable, err := os.Executable()
		if err != nil {
			return binaryLocation{}, err
		}
		resolved, err := filepath.EvalSymlinks(executable)
		return binaryLocation{Binary: binary, Resolved: resolved}, err
	}
	lookPath = exec.LookPath
)

// caskPrefix returns the Homebrew prefix when path lies in the zalki cask's
// Caskroom directory.
func caskPrefix(path string) (string, bool) {
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	for i := 2; i+2 < len(parts); i++ {
		if parts[i] == "Caskroom" && parts[i+1] == "zalki" {
			return string(filepath.Separator) + filepath.Join(parts[:i]...), true
		}
	}
	return "", false
}

func findBrew(prefix string) (string, error) {
	if path, err := lookPath("brew"); err == nil {
		return path, nil
	}
	fallback := filepath.Join(prefix, "bin", "brew")
	if info, err := os.Stat(fallback); err == nil && !info.IsDir() {
		return fallback, nil
	}
	return "", errors.New("zalki was installed with Homebrew but brew was not found; run `" + brewUpgradeHint + "` and then `zalki upgrade --config-only`")
}

type upgradePreview struct {
	install.Report
	BinaryUpgrade string `json:"binary_upgrade"`
}

type upgradeFlags struct {
	dryRun, configOnly, finish bool
}

func newUpgradeCommand(opts *commandOptions) *cobra.Command {
	var f upgradeFlags
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade zalki and refresh client integration configuration",
		Long: "Upgrade the Homebrew-installed zalki binary, then refresh managed client configuration " +
			"with the new build. Settings are backed up before changes.\n\n" +
			"Use --dry-run to preview the configuration changes without upgrading or editing anything. " +
			"For installs not managed by Homebrew, install the new binary yourself and run " +
			"`zalki upgrade --config-only`.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runUpgrade(cmd, opts, f) },
	}
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "preview configuration changes without upgrading the binary or editing settings")
	cmd.Flags().BoolVar(&f.configOnly, "config-only", false, "refresh configuration for the installed binary without upgrading it")
	cmd.Flags().BoolVar(&f.finish, "finish-upgrade", false, "internal: refresh configuration after the binary upgrade")
	_ = cmd.Flags().MarkHidden("finish-upgrade")
	return cmd
}

func runUpgrade(cmd *cobra.Command, opts *commandOptions, f upgradeFlags) error {
	configOnly := f.configOnly || f.finish
	where, err := locateInstallation()
	if err != nil {
		return err
	}
	prefix, cask := caskPrefix(where.Resolved)
	if f.dryRun {
		return previewUpgrade(cmd, opts, where.Binary, cask, configOnly)
	}
	if !configOnly {
		if !cask {
			return errors.New("zalki was not installed with Homebrew, so it cannot upgrade its own binary; " +
				"install the new version the way you installed this one, then run `zalki upgrade --config-only`")
		}
		brew, err := findBrew(prefix)
		if err != nil {
			return err
		}
		if err := runBrewUpgrade(cmd.Context(), brew); err != nil {
			return err
		}
		return finishWithNewBinary(cmd, opts, where.Binary)
	}
	if err := refreshConfiguration(cmd, opts, where.Binary); err != nil {
		return err
	}
	message := configOnlyDone
	if f.finish {
		message = upgradeSuccess
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), message)
	return err
}

func previewUpgrade(cmd *cobra.Command, opts *commandOptions, binary string, cask, configOnly bool) error {
	report, err := manageConfiguration(opts, binary, false)
	if err != nil {
		return err
	}
	step := "unavailable: not a Homebrew install; install the new binary yourself, then run zalki upgrade --config-only"
	switch {
	case configOnly:
		step = "skipped (--config-only)"
	case cask:
		step = brewUpgradeHint
	}
	return encodeOutput(cmd, upgradePreview{Report: report, BinaryUpgrade: step})
}

func manageConfiguration(opts *commandOptions, binary string, apply bool) (install.Report, error) {
	service, err := opts.service()
	if err != nil {
		return install.Report{}, err
	}
	return install.Manage(installDeclarations(opts.registry, opts.snapshot.Paths.ConfigFile.Value), install.Request{
		Action: "upgrade", Binary: binary, StateDir: service.Store.Dir, HomeDir: opts.home, ConfigFile: opts.snapshot.Paths.ConfigFile.Value, Apply: apply,
	})
}

func refreshConfiguration(cmd *cobra.Command, opts *commandOptions, binary string) error {
	if _, err := manageConfiguration(opts, binary, true); err != nil {
		return err
	}
	opts.logger.InfoContext(cmd.Context(), "upgrade.configuration_refreshed")
	return nil
}

// Brew output is hidden on success; on failure its tail explains why. A
// cancelled context stops brew and fails the upgrade before any handoff.
func runBrewUpgrade(ctx context.Context, brew string) error {
	var out bytes.Buffer
	c := exec.CommandContext(ctx, brew, "upgrade", "--cask", caskName)
	c.Stdout, c.Stderr = &out, &out
	c.WaitDelay = brewPipeWaitDelay
	if err := c.Run(); err != nil {
		detail := strings.TrimSpace(out.String())
		if len(detail) > 2000 {
			detail = "..." + detail[len(detail)-2000:]
		}
		return fmt.Errorf("`%s` failed: %w; configuration was not changed\n%s", brewUpgradeHint, err, detail)
	}
	return nil
}

// The running process is the old build and its package directory may be gone,
// so the new build must write the configuration.
func finishWithNewBinary(cmd *cobra.Command, opts *commandOptions, binary string) error {
	args := []string{"upgrade", "--finish-upgrade", "--config", opts.snapshot.Paths.ConfigFile.Value, "--state", opts.snapshot.Paths.StateDir.Value}
	for _, name := range []string{"log-level", "log-file"} {
		if flag := cmd.Flags().Lookup(name); flag != nil && flag.Changed {
			args = append(args, "--"+name+"="+flag.Value.String())
		}
	}
	c := exec.CommandContext(cmd.Context(), binary, args...) //nolint:gosec // G204: installationBinary checked this absolute installed path against os.Executable before the upgrade.
	c.Stdin, c.Stdout, c.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := c.Run(); err != nil {
		return fmt.Errorf("the binary was upgraded but refreshing configuration failed (%w); run `zalki upgrade --config-only` to retry", err)
	}
	return nil
}
