package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
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
	yes, allowUnsandboxed bool
	tree, as              string
}

func newPluginInstallCommand(opts *commandOptions) *cobra.Command {
	var flags installFlags
	cmd := &cobra.Command{
		Use:   "install FOLDER [--as NAME]",
		Short: "Build, pin, grant and enable a local plugin folder after one review",
		Long: "Install reads zalki-plugin.json in FOLDER, copies the folder to a private staging folder, and shows one review screen with one [y/N] question. After yes it builds the copy, pins the result, records the selected access and enables the plugin in one step. Failure installs nothing. Enabled does not mean running: start it with zalki plugins run NAME. NAME is the manifest name, or --as NAME to install one more instance of the same plugin with its own data, lease and access.\n\n" +
			installWarning +
			"\n\nThe plugin gets the standard access it asks for (changes, receive) in every live session (scope all). Extended access (send, read) is not installed here; the install says how to give it. Without a terminal, --yes needs --tree-sha256 and --allow-unsandboxed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			return (&pluginInstaller{cmd: cmd, store: service.Store, flags: flags}).run(args[0])
		},
	}
	cmd.Flags().StringVar(&flags.as, "as", "", "local name of this instance (default: the manifest name)")
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "install without the review screen; needs the flags below")
	cmd.Flags().StringVar(&flags.tree, "tree-sha256", "", "expected snapshot hash of FOLDER (with --yes)")
	cmd.Flags().BoolVar(&flags.allowUnsandboxed, "allow-unsandboxed", false, "accept that the build and plugin run without a sandbox (with --yes)")
	return cmd
}

type pluginInstaller struct {
	cmd     *cobra.Command
	store   bus.Store
	flags   installFlags
	install *pluginfeed.PluginInstall
	source  plugins.InstallSource
	tool    plugins.InstallTool
	folder  string
	name    string // the local instance name: --as, or the manifest name
}

func (p *pluginInstaller) say(format string, args ...any) {
	_, _ = fmt.Fprintf(p.cmd.OutOrStdout(), format, args...)
}

func (p *pluginInstaller) run(folder string) (err error) {
	if !p.flags.yes && (p.flags.tree != "" || p.flags.allowUnsandboxed) {
		return errors.New("--tree-sha256 and --allow-unsandboxed need --yes; without --yes, answer on the review screen")
	}
	defer restoreSignalsOnCancel(p.cmd.Context())()
	p.say("Preparing...\n")
	if p.folder, err = plugins.ResolveInstallSource(folder); err != nil {
		return err
	}
	manifest, err := plugins.ReadInstallManifest(p.folder)
	if err != nil {
		return err
	}
	if p.name = manifest.Name; p.flags.as != "" {
		p.name = p.flags.as
	}
	if manifest.Access.Send || manifest.Access.Read {
		return extendedInstallError(p.name, manifest.Access)
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
		p.say("Cancelled. Nothing was installed.\n")
		return nil
	}
	return err
}

// prepare copies the folder and checks the copy, which is what is built and
// reviewed. The first read only gave the name; a manifest that changed its
// name or access since then is refused, and send/read are refused on the copy.
func (p *pluginInstaller) prepare(first plugins.InstallManifest) error {
	var err error
	if p.source, err = plugins.SnapshotInstallSource(p.folder, p.install.StagingDir()); err != nil {
		return err
	}
	frozen := p.source.Manifest
	if frozen.Name != first.Name || frozen.Access != first.Access {
		return errors.New("plugin install: the folder changed while it was copied; nothing was installed")
	}
	if frozen.Access.Send || frozen.Access.Read {
		return extendedInstallError(p.name, frozen.Access)
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
	fmt.Fprintf(&b, "Snapshot: %s\n", p.source.TreeSHA256)
	fmt.Fprintf(&b, "Read the copied source: %s\n", plainText(p.source.Dir))
	fmt.Fprintf(&b, "Purpose (author claim): %s\n", plainText(m.Purpose))
	if len(m.Build) > 0 {
		fmt.Fprintf(&b, "Build: %s\n", quotedList(m.Build))
		fmt.Fprintf(&b, "Tool: %s (go launcher; its hash is checked for changes, the compiler is not pinned)\n", plainText(p.tool.Path))
		b.WriteString("Build env: only PATH from your environment; private caches, GOTOOLCHAIN=local, GOENV=off, GOWORK=off, -buildvcs=false, -mod=readonly (vendor/ is ignored), cgo off, no network (GOPROXY=off), 2 CPUs. go.mod may use only the standard library. VCS folders such as .git are not copied.\n")
	} else {
		b.WriteString("Build: none (ready-made executable)\n")
	}
	fmt.Fprintf(&b, "Artifact: %s, pinned by SHA256 after the build\n", plainText(m.Artifact))
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

func (p *pluginInstaller) review() (pluginfeed.PluginAccess, bool, error) {
	requests := p.source.Manifest.Access
	access := pluginfeed.PluginAccess{Status: requests.Status, Receive: requests.Receive}
	screen := p.header() + installAccessLine(requests)
	if p.flags.yes {
		return access, true, p.reviewFlags(screen)
	}
	in, out := p.cmd.InOrStdin(), p.cmd.OutOrStdout()
	choice, restore := installTerminal(in)
	defer restore() // also after a panic, so the terminal never stays raw
	// Ctrl-C, SIGTERM or SIGHUP cancel the context; the reader then stops waiting.
	keys := newContextReader(p.cmd.Context(), in)
	var yes bool
	var err error
	switch choice {
	case installRawKeys:
		yes, err = askInstallRaw(keys, out, screen)
	case installPlainLines:
		yes, err = askInstallLines(keys, out, screen)
	case installNoTerminal:
		err = errors.New("plugins install needs a terminal for its review screen; use --yes with --tree-sha256 and --allow-unsandboxed instead")
	}
	return access, yes, err
}

// reviewFlags checks the non-interactive choices and prints the same screen.
func (p *pluginInstaller) reviewFlags(screen string) error {
	switch {
	case p.flags.tree != p.source.TreeSHA256:
		return fmt.Errorf("--tree-sha256 does not match the folder snapshot %s; nothing was installed", p.source.TreeSHA256)
	case !p.flags.allowUnsandboxed:
		return errors.New("--yes needs --allow-unsandboxed: the build and plugin run without a sandbox")
	}
	p.say("%s", screen)
	p.say("Accepted by --yes.\n")
	return nil
}

func (p *pluginInstaller) finish(access pluginfeed.PluginAccess) error {
	m := p.source.Manifest
	receipt := pluginfeed.PluginInstallReceipt{API: m.API, ID: m.ID, Version: m.Version, Source: p.folder, TreeSHA256: p.source.TreeSHA256, ManifestSHA256: p.source.ManifestSHA256, Build: m.Build, Artifact: m.Artifact, InstalledAt: model.Timestamp()}
	ctx := p.cmd.Context()
	if len(m.Build) > 0 {
		installStep("build")
		if ctx.Err() != nil {
			return errInstallCancelled
		}
		p.say("Building...\n")
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
			p.say("%s\n", plainLines(plugins.TailInstallLog(log, 20)))
			return fmt.Errorf("build failed: %w. Nothing was installed. Full log: %s (removed by the next install of %s or by plugins remove %s)", err, log, p.name, p.name)
		}
	}
	p.say("Verifying...\n")
	artifact, err := plugins.StagedInstallArtifact(p.source.Dir, m.Artifact)
	if err != nil {
		return fmt.Errorf("%w. Nothing was installed", err)
	}
	p.say("Enabling...\n")
	installStep("commit")
	// Point of no return: this is the last check, after all output. Once Commit
	// starts (including its wait for the Store lock) the install completes and
	// is reported as installed, so the success text below is always true.
	if ctx.Err() != nil {
		return errInstallCancelled
	}
	if err := installCommitError(p.name)(p.install.Commit(artifact, m.Args, access, receipt)); err != nil {
		return err
	}
	p.say("Installed and enabled %s. Access: %s. Not running yet: start it with zalki plugins run %s\n", p.name, describeAccess("all", access), p.name)
	return nil
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
