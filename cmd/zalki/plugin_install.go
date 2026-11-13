package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"github.com/zalkilabs/zalki/internal/plugins"
)

// installWarning is the one warning on the install screen. It keeps every
// statement of the build warning and of pluginTrustNotice in two lines.
const installWarning = "Warning: no sandbox. The build runs now and the plugin runs later with your OS user access and normal environment; Zalki cannot undo what they do to your files.\n" +
	"The plugin can read and change all your Zalki state. Access filters only what Zalki exposes, not file access. Install only code you trust."

type installChoice int

const (
	installNoTerminal installChoice = iota
	installRawKeys
	installPlainLines
)

// installTerminal decides how to ask the owner. Tests replace it.
var installTerminal = func(in io.Reader) (installChoice, func()) {
	file, ok := in.(*os.File)
	if !ok || !isInstallTerminal(file) {
		return installNoTerminal, func() {}
	}
	if os.Getenv("TERM") != "dumb" {
		if restore, err := rawInstallTerminal(file); err == nil {
			return installRawKeys, restore
		}
	}
	return installPlainLines, func() {}
}

type installFlags struct {
	yes, allowUnsandboxed, details bool
	tree, as                       string
}

func newPluginInstallCommand(opts *commandOptions) *cobra.Command {
	var flags installFlags
	cmd := &cobra.Command{
		Use:   "install FOLDER [--as NAME]",
		Short: "Build, pin, grant and enable a local plugin folder after one review",
		Long: "Install reads zalki-plugin.json in FOLDER, copies the folder to a private staging folder, and shows one review screen with one [y/N] question. After yes it builds the copy, pins the result, records the selected access and enables the plugin in one step. A failed install preserves existing state; a partial commit is reported explicitly. Enabled does not mean running: start it with zalki plugins run NAME. NAME is the manifest name, or --as NAME to install one more instance of the same plugin with its own data, lease and access.\n\n" +
			installWarning +
			"\n\nThe plugin gets the standard access it asks for (status, receive) in every live session (scope all). Extended SEND/READ is optional and limited to named running sessions chosen in your terminal. --yes refuses any manifest declaring either kind. Without a terminal, --yes needs --tree-sha256 and --allow-unsandboxed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			return (&pluginInstaller{cmd: cmd, store: service.Store, flags: flags, home: opts.home}).run(args[0])
		},
	}
	cmd.Flags().BoolVar(&flags.details, "details", false, "show the captured source, build and trust details before the question")
	cmd.Flags().StringVar(&flags.as, "as", "", "local name of this instance (default: the manifest name)")
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "install without the review screen; needs the flags below")
	cmd.Flags().StringVar(&flags.tree, "tree-sha256", "", "expected snapshot hash of FOLDER (with --yes)")
	cmd.Flags().BoolVar(&flags.allowUnsandboxed, "allow-unsandboxed", false, "accept that the build and plugin run without a sandbox (with --yes)")
	return cmd
}

type pluginInstaller struct {
	cmd                                 *cobra.Command
	store                               bus.Store
	flags                               installFlags
	install                             *pluginfeed.PluginInstall
	source                              plugins.InstallSource
	tool                                plugins.InstallTool
	folder                              string
	name                                string // the local instance name: --as, or the manifest name
	home                                string
	installerActor, installerCapability string
	terminalReviewed                    bool
}

func (p *pluginInstaller) say(format string, args ...any) error {
	if _, err := fmt.Fprintf(p.cmd.OutOrStdout(), format, args...); err != nil {
		return errors.New("could not write plugin install output")
	}
	return nil
}

func (p *pluginInstaller) run(folder string) (err error) {
	// This runs after staging-close error joins too; normal diagnostics stay exact.
	defer func() { err = pluginSourceError(err) }()
	if !p.flags.yes && (p.flags.tree != "" || p.flags.allowUnsandboxed) {
		return errors.New("--tree-sha256 and --allow-unsandboxed need --yes; without --yes, answer on the review screen")
	}
	defer restoreSignalsOnCancel(p.cmd.Context())()
	if err := p.say("Preparing...\n"); err != nil {
		return err
	}
	p.installerActor, p.installerCapability = os.Getenv("ZALKI_ACTOR"), os.Getenv("ZALKI_CAPABILITY")
	manifest, err := p.readSource(folder)
	if err != nil {
		return err
	}
	if p.name = manifest.Name; p.flags.as != "" {
		p.name = p.flags.as
	}
	if p.install, err = pluginfeed.NewPluginFeed(p.store, p.name).BeginPluginInstall(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, p.install.Close()) }()
	if err := p.prepare(manifest); err != nil {
		return err
	}
	access, ok, err := p.review()
	switch {
	case err == nil && ok:
		err = p.finish(access)
	case err == nil:
		err = errInstallCancelled
	}
	if cancelled(err) {
		// The command ends here and the process exits: nothing runs or reads after this.
		return p.say("Cancelled. Nothing was installed.\n")
	}
	return err
}

// prepare copies the folder and checks the copy, which is what is built and
// reviewed. The first read only gave the name; a manifest that changed its
// name/id/version is refused. Access, review and --yes guards use the copy only.
func (p *pluginInstaller) prepare(first plugins.InstallManifest) error {
	var err error
	installStep("snapshot")
	if p.cmd != nil && p.cmd.Context() != nil && p.cmd.Context().Err() != nil {
		return errInstallCancelled
	}
	if p.source, err = plugins.SnapshotInstallSource(p.folder, p.install.StagingDir()); err != nil {
		return err
	}
	frozen := p.source.Manifest
	if frozen.Name != first.Name || frozen.ID != first.ID || frozen.Version != first.Version {
		return errors.New("plugin install: the folder changed while it was copied; nothing was installed")
	}
	if p.flags.yes && (frozen.Access.Send || frozen.Access.Read) {
		return p.extendedYesError(frozen.Access)
	}
	if len(p.source.Manifest.Build) > 0 {
		p.tool, err = plugins.ResolveInstallTool(p.source.Manifest.Build[0])
	}
	return err
}

func (p *pluginInstaller) header() string {
	m := p.source.Manifest
	var b strings.Builder
	fmt.Fprintf(&b, "Install %q (%s %s, author's claim)\n", p.name, m.ID, plainText(m.Version))
	fmt.Fprintf(&b, "Source: %s\n", plainText(p.folder))
	fmt.Fprintf(&b, "Snapshot: %s\nManifest SHA256: %s\n", p.source.TreeSHA256, p.source.ManifestSHA256)
	fmt.Fprintf(&b, "Read the copied source: %s\n", plainText(p.source.Dir))
	fmt.Fprintf(&b, "Purpose (author claim): %s\n", plainText(m.Purpose))
	if len(m.Build) > 0 {
		fmt.Fprintf(&b, "Build: %s\n", quotedList(m.Build))
		fmt.Fprintf(&b, "Tool: %s (go launcher; its hash is checked for changes, the compiler is not pinned)\nTool SHA256: %s\n", plainText(p.tool.Path), p.tool.SHA256)
		b.WriteString("Build env: only PATH from your environment; private caches, GOTOOLCHAIN=local, GOENV=off, GOWORK=off, -buildvcs=false, -mod=readonly (vendor/ is ignored), cgo off, no network (GOPROXY=off), 2 CPUs. go.mod may use only the standard library. VCS folders such as .git are not copied.\n")
	} else {
		b.WriteString("Build: none (ready-made executable)\n")
	}
	fmt.Fprintf(&b, "Artifact: %s; artifact pin: pending build\n", plainText(m.Artifact))
	if len(m.Args) > 0 {
		fmt.Fprintf(&b, "Run arguments: %s\n", quotedList(m.Args))
	} else {
		b.WriteString("Run arguments: none\n")
	}
	b.WriteString("Publisher proof: none (local folder)\n")
	b.WriteString(installWarning + "\n")
	return b.String()
}

// quotedList shows each item quoted, so spaces and control characters are
// visible: ["go", "build", "-o", "plugin", "."].
func quotedList(items []string) string {
	quoted := make([]string, 0, len(items))
	for _, item := range items {
		quoted = append(quoted, strconv.Quote(item))
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func (p *pluginInstaller) finish(access pluginfeed.PluginAccess) error {
	m := p.source.Manifest
	receipt := pluginfeed.PluginInstallReceipt{API: m.API, ID: m.ID, Version: m.Version, Source: p.folder, TreeSHA256: p.source.TreeSHA256, ManifestSHA256: p.source.ManifestSHA256, Build: m.Build, Artifact: m.Artifact}
	declared, err := json.Marshal(m.Access) // the reviewed copy's declared access
	if err != nil {
		return err
	}
	receipt.Access = declared
	ctx := p.cmd.Context()
	if len(m.Build) > 0 {
		installStep("build")
		if ctx.Err() != nil {
			return errInstallCancelled
		}
		if err := p.say("Building...\n"); err != nil {
			return err
		}
		receipt.Tool, receipt.ToolSHA256 = p.tool.Path, p.tool.SHA256
		log := filepath.Join(p.install.StagingDir(), "build.log")
		if err := plugins.RunInstallBuild(ctx, p.tool, m.Build, p.source.Dir, log); err != nil {
			if ctx.Err() != nil {
				return errInstallCancelled // no log is kept: Close removes the staging folder
			}
			// Keep only the log in its fixed place; the copied source is removed.
			if err := os.RemoveAll(p.source.Dir); err == nil {
				p.install.KeepBuildLog()
			}
			err = errors.Join(err, p.say("%s\n", plainLines(plugins.TailInstallLog(log, 20))))
			return fmt.Errorf("build failed: %w. Nothing was installed. Full log: %s (removed by the next install of %s or by plugins remove %s)", err, log, p.name, p.name)
		}
	}
	if err := p.say("Verifying...\n"); err != nil {
		return err
	}
	artifact, err := plugins.StagedInstallArtifact(p.source.Dir, m.Artifact)
	if err != nil {
		return fmt.Errorf("%w. Nothing was installed", err)
	}
	if err := p.say("Enabling...\n"); err != nil {
		return err
	}
	installStep("commit")
	// Point of no return: this is the last check, after all output. Once Commit
	// starts (including its wait for the Store lock) the install completes and
	// is reported as installed, so the success text below is always true.
	if ctx.Err() != nil {
		return errInstallCancelled
	}
	if err := installCommitError(p.name)(p.install.Commit(artifact, m.Args, access, receipt, p.installerActor, p.installerCapability, p.terminalReviewed)); err != nil {
		return err
	}
	return p.reportInstalled()
}

// installCommitError says what a failed commit left behind. When the record
// may be published (kept), it must never say that nothing was installed.
func installCommitError(name string) func(bool, error) error {
	return func(kept bool, err error) error {
		switch {
		case err == nil:
			return nil
		case kept:
			return fmt.Errorf("enabling was not confirmed: %w. The plugin may be installed and enabled; check it with zalki plugins status %s (remove it with zalki plugins disable %s and zalki plugins remove %s)", err, name, name, name)
		default:
			return fmt.Errorf("enabling failed: %w. Nothing was installed", err)
		}
	}
}
