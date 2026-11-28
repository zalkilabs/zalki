package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"

	"github.com/spf13/cobra"
)

func newSetupCommand(opts *commandOptions) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use: "setup", Short: "Preview the configuration changes, or write drafts of them to a folder", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if out == "" {
				return previewInstall(cmd, opts)
			}
			return writeConfigurationDrafts(cmd, opts, out)
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "write drafts of the configuration files to this empty folder; without it, print the preview")
	return cmd
}

// changeFlags are the flags of the commands that change client settings.
type changeFlags struct {
	dryRun, yes bool
}

func addChangeFlags(cmd *cobra.Command, f *changeFlags) {
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print what would change and change nothing")
	cmd.Flags().BoolVar(&f.yes, "yes", false, "change the files without asking")
	cmd.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if strings.Contains(err.Error(), "unknown flag: --apply") {
			return fmt.Errorf("--apply is no longer needed; run zalki %s and answer the question, or use --yes", cmd.Name())
		}
		return err
	})
}

func newInstallCommand(opts *commandOptions) *cobra.Command {
	var f changeFlags
	cmd := &cobra.Command{
		Use: "install", Short: "Configure your agent clients for Zalki, keeping backups", Args: cobra.NoArgs,
		Long: "Lists the files Zalki will change, asks one question, then changes them and keeps a backup of each. " +
			"Use --yes to skip the question, or --dry-run to print what would change.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if f.dryRun {
				return previewInstall(cmd, opts)
			}
			var shown []string
			if !f.yes {
				ok, notes, err := confirmInstall(cmd, opts)
				if err != nil || !ok {
					return err
				}
				shown = notes
			}
			return applyConfiguration(cmd, opts, shown)
		},
	}
	addChangeFlags(cmd, &f)
	return cmd
}

// installRequest is the request every install step starts from.
func installRequest(opts *commandOptions, service *bus.Service, action string) (install.Request, error) {
	binary, err := installationBinary()
	if err != nil {
		return install.Request{}, err
	}
	request := install.Request{Action: action, Binary: binary, StateDir: service.Store.Dir(), HomeDir: opts.home}
	if action == "install" {
		request.ConfigFile = opts.snapshot.Paths.ConfigFile.Value
	}
	return request, nil
}

// previewInstall prints what an install would change and changes nothing.
func previewInstall(cmd *cobra.Command, opts *commandOptions) error {
	service, err := opts.service()
	if err != nil {
		return err
	}
	request, err := installRequest(opts, service, "install")
	if err != nil {
		return err
	}
	report, err := install.Manage(installDeclarations(opts.registry, opts.snapshot.Paths.ConfigFile.Value), request)
	if err != nil {
		return err
	}
	return errors.Join(encodeOutput(cmd, report), skippedClientsError(report))
}

// confirmInstall lists the files an install would change and asks once. It
// returns false, with "Nothing changed." printed, when the answer is not yes. It
// also returns the notes it printed before the question.
func confirmInstall(cmd *cobra.Command, opts *commandOptions) (bool, []string, error) {
	service, err := opts.service()
	if err != nil {
		return false, nil, err
	}
	request, err := installRequest(opts, service, "install")
	if err != nil {
		return false, nil, err
	}
	report, err := install.Manage(installDeclarations(opts.registry, opts.snapshot.Paths.ConfigFile.Value), request)
	if err != nil {
		return false, nil, err
	}
	ok, err := confirmChange(cmd, "install", report, opts.home)
	if !ok || len(report.Paths) == 0 {
		return ok, nil, err
	}
	return ok, report.Notes, err
}

// pathsByClient returns one line for each client, "Codex: ~/.codex/config.toml, ...",
// in the order of the client names. A path under the home folder starts with ~.
// Paths that belong to no client come last, under "Zalki".
func pathsByClient(report install.Report, home string) []string {
	byLabel := map[string][]string{}
	for _, path := range report.Paths {
		label := report.Clients[path]
		if label == "" {
			label = "Zalki"
		}
		if rest, ok := strings.CutPrefix(path, home+string(filepath.Separator)); ok && home != "" {
			path = "~" + string(filepath.Separator) + rest
		}
		byLabel[label] = append(byLabel[label], path)
	}
	labels := make([]string, 0, len(byLabel))
	for label := range byLabel {
		labels = append(labels, label)
	}
	sort.Slice(labels, func(i, j int) bool {
		if labels[i] == "Zalki" || labels[j] == "Zalki" {
			return labels[j] == "Zalki" && labels[i] != "Zalki"
		}
		return labels[i] < labels[j]
	})
	lines := make([]string, 0, len(labels))
	for _, label := range labels {
		lines = append(lines, label+": "+strings.Join(byLabel[label], ", "))
	}
	return lines
}

// confirmChange asks whether to change the listed files. With nothing to
// change it does not ask. Without a terminal it refuses and names --yes.
func confirmChange(cmd *cobra.Command, action string, report install.Report, home string) (bool, error) {
	if len(report.Paths) == 0 {
		return true, nil
	}
	choice, restore := installTerminal(cmd.InOrStdin())
	defer restore()
	if choice == installNoTerminal {
		return false, fmt.Errorf("zalki %s needs a terminal to ask; run zalki %s --yes to change the files without asking", action, action)
	}
	for _, note := range report.Notes {
		if err := sayLine(cmd, note); err != nil {
			return false, err
		}
	}
	if err := sayLine(cmd, "Zalki will change these files and keep a backup of each:"); err != nil {
		return false, err
	}
	for _, line := range pathsByClient(report, home) {
		if err := sayLine(cmd, "  "+line); err != nil {
			return false, err
		}
	}
	yes, err := askOnce(cmd.Context(), cmd, choice == installRawKeys, "Apply these changes? [y/N]")
	if err != nil || !yes {
		if err == nil {
			err = sayLine(cmd, "Nothing changed.")
		}
		return false, err
	}
	return true, nil
}

// applyConfiguration changes the files. Notes in shown were printed before the
// question and are not printed again.
func applyConfiguration(cmd *cobra.Command, opts *commandOptions, shown []string) error {
	service, err := opts.service()
	if err != nil {
		return err
	}
	request, err := installRequest(opts, service, "install")
	if err != nil {
		return err
	}
	request.Apply = true
	report, err := install.Manage(installDeclarations(opts.registry, opts.snapshot.Paths.ConfigFile.Value), request)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Config file:", opts.snapshot.Paths.ConfigFile.Value); err != nil {
		return err
	}
	for _, path := range report.Paths {
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Updated:", path); err != nil {
			return err
		}
	}
	for _, note := range report.Notes {
		if slices.Contains(shown, note) {
			continue
		}
		if _, err := fmt.Fprintln(cmd.OutOrStdout(), note); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "Existing settings were preserved and backed up. Review/trust integrations in each configured client, then launch normally. Check doctor for adapter and listener requirements.")
	if err != nil {
		return err
	}
	if err := printInstallNotices(opts.registry, report.Configured, func(line string) error {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), line)
		return err
	}); err != nil {
		return err
	}
	if target := newLoginItemTarget(opts, service.Store.Dir()); target != nil {
		if err := target.rootSetup(cmd, service.Store); err != nil {
			return errors.Join(err, skippedClientsError(report))
		}
	}
	return skippedClientsError(report)
}

// skippedClientsError makes a run that skipped a client because its file cannot
// be used exit non-zero, after its output has said which file and what to do.
func skippedClientsError(report install.Report) error {
	if len(report.Unusable) == 0 {
		return nil
	}
	return fmt.Errorf("skipped %s: fix the file named above, then run the command again", strings.Join(report.Unusable, ", "))
}

func writeConfigurationDrafts(cmd *cobra.Command, opts *commandOptions, out string) error {
	service, err := opts.service()
	if err != nil {
		return err
	}
	binary, err := installationBinary()
	if err != nil {
		return err
	}
	if err := install.WriteSetup(installDeclarations(opts.registry, opts.snapshot.Paths.ConfigFile.Value), binary, service.Store.Dir(), out, opts.home, opts.snapshot.Paths.ConfigFile.Value); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Config file:", opts.snapshot.Paths.ConfigFile.Value); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(cmd.OutOrStdout(), "Configuration drafts written to", out); err != nil {
		return err
	}
	_, err = fmt.Fprintln(cmd.OutOrStdout(), "To apply: zalki install")
	return err
}
