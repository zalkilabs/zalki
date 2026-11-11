package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	"The plugin can read and change all your Zalki state. Your session choices filter only the Zalki feed, not file access. Install only code you trust."

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
	yes, allowUnsandboxed, inboxRead bool
	tree                             string
	grants                           pluginfeed.PluginInstallGrants
}

func newPluginInstallCommand(opts *commandOptions) *cobra.Command {
	var flags installFlags
	cmd := &cobra.Command{
		Use:   "install FOLDER",
		Short: "Build, pin, grant and enable a local plugin folder after one review",
		Long: "Install reads zalki-plugin.json in FOLDER, copies the folder to a private staging folder, and shows one review screen with one [y/N] question. After yes it builds the copy, pins the result, records the selected access and enables the plugin in one step. Failure installs nothing. Enabled does not mean running: start it with zalki plugins run NAME.\n\n" +
			installWarning +
			"\n\nWithout a terminal, --yes needs --tree-sha256, --allow-unsandboxed and explicit access flags. --yes never grants everything the plugin asks for.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			service, err := opts.service()
			if err != nil {
				return err
			}
			flags.grants.InboxRead = flags.inboxRead
			return (&pluginInstaller{cmd: cmd, store: service.Store, flags: flags}).run(args[0])
		},
	}
	cmd.Flags().BoolVar(&flags.yes, "yes", false, "install without the review screen; needs the flags below")
	cmd.Flags().StringVar(&flags.tree, "tree-sha256", "", "expected snapshot hash of FOLDER (with --yes)")
	cmd.Flags().BoolVar(&flags.allowUnsandboxed, "allow-unsandboxed", false, "accept that the build and plugin run without a sandbox (with --yes)")
	cmd.Flags().StringArrayVar(&flags.grants.Sessions, "session", nil, "exact session the plugin sees changes from (repeat; with --yes)")
	cmd.Flags().StringArrayVar(&flags.grants.Read, "read", nil, "exact session whose message bodies it may read (repeat; with --yes)")
	cmd.Flags().StringArrayVar(&flags.grants.Send, "send", nil, "exact session it may send to (repeat; with --yes)")
	cmd.Flags().StringArrayVar(&flags.grants.InboxFrom, "accept-from", nil, "exact session allowed to message it (repeat; with --yes)")
	cmd.Flags().BoolVar(&flags.inboxRead, "inbox-read", false, "allow reading its own inbox (with --yes)")
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
}

func (p *pluginInstaller) say(format string, args ...any) {
	_, _ = fmt.Fprintf(p.cmd.OutOrStdout(), format, args...)
}

func (p *pluginInstaller) run(folder string) (err error) {
	if !p.flags.yes && (p.flags.tree != "" || p.flags.allowUnsandboxed || p.flags.grants.InboxRead || len(slices.Concat(p.flags.grants.Sessions, p.flags.grants.Read, p.flags.grants.Send, p.flags.grants.InboxFrom)) > 0) {
		return errors.New("access, --tree-sha256 and --allow-unsandboxed flags need --yes; without --yes, choose on the review screen")
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
	if p.install, err = pluginfeed.NewPluginFeed(p.store, manifest.Name).BeginPluginInstall(); err != nil {
		return err
	}
	defer func() { err = errors.Join(err, p.install.Close()) }()
	if err := p.prepare(manifest.Name); err != nil {
		return err
	}
	grants, ok, err := p.review()
	switch {
	case err == nil && ok:
		err = p.finish(grants)
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

func (p *pluginInstaller) prepare(name string) error {
	var err error
	if p.source, err = plugins.SnapshotInstallSource(p.folder, p.install.StagingDir()); err != nil {
		return err
	}
	if p.source.Manifest.Name != name {
		return errors.New("plugin install: the folder changed while it was copied; nothing was installed")
	}
	if len(p.source.Manifest.Build) > 0 {
		p.tool, err = plugins.ResolveInstallTool(p.source.Manifest.Build[0])
	}
	return err
}

func (p *pluginInstaller) header() string {
	m := p.source.Manifest
	var b strings.Builder
	fmt.Fprintf(&b, "Install %s\n", m.Name)
	fmt.Fprintf(&b, "Source: %s\n", plainText(p.folder))
	fmt.Fprintf(&b, "Snapshot: %s\n", p.source.TreeSHA256)
	fmt.Fprintf(&b, "Read the copied source: %s\n", plainText(p.source.Dir))
	fmt.Fprintf(&b, "Purpose (author claim): %s\n", plainText(m.Purpose))
	if len(m.Build) > 0 {
		args := make([]string, 0, len(m.Build))
		for _, arg := range m.Build {
			args = append(args, plainText(arg))
		}
		fmt.Fprintf(&b, "Build: %s\n", strings.Join(args, " "))
		fmt.Fprintf(&b, "Tool: %s\n", plainText(p.tool.Path))
	} else {
		b.WriteString("Build: none (ready-made executable)\n")
	}
	fmt.Fprintf(&b, "Artifact: %s, pinned by SHA256 after the build\n", plainText(m.Artifact))
	b.WriteString("Publisher proof: none (local folder)\n")
	b.WriteString(installWarning + "\n")
	return b.String()
}

// sessions lists live sessions; Store.List already leaves out retired and exited ones.
func (p *pluginInstaller) sessions() ([]model.Session, error) {
	all, err := p.store.List()
	if err != nil {
		return nil, err
	}
	slices.SortStableFunc(all, func(a, b model.Session) int { return strings.Compare(b.UpdatedAt, a.UpdatedAt) })
	return all[:min(len(all), 50)], nil
}

func (p *pluginInstaller) review() (pluginfeed.PluginInstallGrants, bool, error) {
	sessions, err := p.sessions()
	if err != nil {
		return pluginfeed.PluginInstallGrants{}, false, err
	}
	screen := newInstallScreen(p.header(), p.source.Manifest.Requests, sessions)
	if p.flags.yes {
		return p.reviewFlags(screen)
	}
	if len(sessions) == 0 {
		return pluginfeed.PluginInstallGrants{}, false, errors.New("no Zalki sessions are registered, so there is nothing to grant; nothing was installed")
	}
	in, out := p.cmd.InOrStdin(), p.cmd.OutOrStdout()
	choice, restore := installTerminal(in)
	// Ctrl-C, SIGTERM or SIGHUP cancel the context; the reader then stops waiting.
	keys := newContextReader(p.cmd.Context(), in)
	var yes bool
	switch choice {
	case installRawKeys:
		yes, err = screen.chooseRaw(keys, out)
		restore()
	case installPlainLines:
		yes, err = screen.chooseLines(keys, out)
	case installNoTerminal:
		err = errors.New("plugins install needs a terminal for its review screen; use --yes with explicit flags instead")
	}
	return screen.grants(), yes, err
}

// reviewFlags checks the non-interactive choices and prints the same plan.
func (p *pluginInstaller) reviewFlags(screen *installScreen) (pluginfeed.PluginInstallGrants, bool, error) {
	g, r := p.flags.grants, p.source.Manifest.Requests
	switch {
	case p.flags.tree != p.source.TreeSHA256:
		return g, false, fmt.Errorf("--tree-sha256 does not match the folder snapshot %s; nothing was installed", p.source.TreeSHA256)
	case !p.flags.allowUnsandboxed:
		return g, false, errors.New("--yes needs --allow-unsandboxed: the build and plugin run without a sandbox")
	case unrequestedInstallFlags(g, r):
		return g, false, errors.New("an access flag asks for something the plugin did not request; nothing was installed")
	}
	known := map[string]int{}
	for i, session := range screen.sessions {
		known[session.ID] = i
	}
	for j, kind := range screen.kinds {
		for _, id := range *kind.field(&g) {
			i, ok := known[id]
			if !ok {
				return g, false, fmt.Errorf("unknown session %s; nothing was installed", plainText(id))
			}
			screen.selected[[2]int{i, j}] = true
		}
	}
	screen.inboxOn = g.InboxRead
	if notice := screen.check(); notice != "" {
		return g, false, errors.New(notice + " Nothing was installed.")
	}
	p.say("%s", strings.TrimSuffix(screen.render(screenPlan), "Install and enable with these choices? [y/N] "))
	p.say("Accepted by --yes.\n")
	return screen.grants(), true, nil
}

func unrequestedInstallFlags(g pluginfeed.PluginInstallGrants, r plugins.InstallRequests) bool {
	return (len(g.Read) > 0 && !r.Read) || (len(g.Send) > 0 && !r.Send) || (len(g.InboxFrom) > 0 && !r.AcceptFrom) || (g.InboxRead && !r.InboxRead)
}

func (p *pluginInstaller) finish(grants pluginfeed.PluginInstallGrants) error {
	m := p.source.Manifest
	receipt := pluginfeed.PluginInstallReceipt{Source: p.folder, TreeSHA256: p.source.TreeSHA256, ManifestSHA256: p.source.ManifestSHA256, Build: m.Build, Artifact: m.Artifact, InstalledAt: model.Timestamp()}
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
			return fmt.Errorf("build failed: %w. Nothing was installed. Full log: %s (removed by the next install of %s or by plugins remove %s)", err, log, m.Name, m.Name)
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
	if err := installCommitError(m.Name)(p.install.Commit(artifact, m.Args, grants, receipt)); err != nil {
		return err
	}
	p.say("Installed and enabled %s. Access: %s. Not running yet: start it with zalki plugins run %s\n", m.Name, describeInstallGrants(grants), m.Name)
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

func describeInstallGrants(g pluginfeed.PluginInstallGrants) string {
	parts := []string{fmt.Sprintf("changes from %d session(s)", len(g.Sessions))}
	for _, kind := range []struct {
		label string
		ids   []string
	}{{"read bodies from", g.Read}, {"send to", g.Send}, {"accept messages from", g.InboxFrom}} {
		if len(kind.ids) > 0 {
			parts = append(parts, fmt.Sprintf("%s %d session(s)", kind.label, len(kind.ids)))
		}
	}
	if g.InboxRead {
		parts = append(parts, "read its own inbox")
	}
	return strings.Join(parts, ", ")
}
