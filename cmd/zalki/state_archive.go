package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
)

const archiveOfflineWarning = "Close all agent sessions and stop background plugins first. Sessions running or started during the move can create a second state and damage its journal."
var errArchiveNoState = errors.New("no state to archive")

const archiveQuestion = "Move this Zalki state to the archive? [y/N] "

// Archive never constructs a Service or opens the diagnostic file in the state being moved.
func newStateCommand(opts *commandOptions) *cobra.Command {
	group := &cobra.Command{
		Use: "state", Short: "Manage offline Zalki state",
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			opts.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
			return nil
		},
	}
	group.AddCommand(&cobra.Command{
		Use: "archive", Short: "Move stopped Zalki state into a private sibling archive", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := opts.resolvedPaths()
			if err != nil {
				return err
			}
			request := archiveRequest{source: paths.StateDir.Value, origin: paths.StateDir.Origin,
				home: opts.home, config: paths.ConfigDir, output: cmd.OutOrStdout(), confirm: func() (bool, error) {
					return confirmStateArchive(cmd)
				}}
			return archiveState(cmd.Context(), request, defaultArchiveOps())
		},
	})
	return group
}

func confirmStateArchive(cmd *cobra.Command) (bool, error) {
	input, ok := cmd.InOrStdin().(*os.File)
	if !ok || !isInstallTerminal(input) {
		return false, archiveRefusal("confirmation needs your own terminal")
	}
	line, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	if errors.Is(err, io.EOF) {
		return false, nil
	}
	return strings.EqualFold(strings.TrimSpace(line), "y") || strings.EqualFold(strings.TrimSpace(line), "yes"), nil
}

type archiveRequest struct {
	source, origin, home, config string
	output                      io.Writer
	confirm                     func() (bool, error)
}

type archiveOps struct {
	background func(context.Context, string) error
	rename     func(string, string) error
	sync       func(string) error
	now        func() time.Time
	id         func() string
}

func defaultArchiveOps() archiveOps {
	return archiveOps{background: archiveBackgroundStopped, rename: os.Rename,
		sync: fsutil.SyncDir, now: time.Now, id: model.NewUUID}
}

func archiveRefusal(reason string) error {
	return fmt.Errorf("State was not moved: %s. Close all agent sessions and stop background plugins, then run zalki state archive again", reason)
}

func archiveState(ctx context.Context, request archiveRequest, ops archiveOps) error {
	plan, err := prepareStateArchive(request, ops)
	if errors.Is(err, errArchiveNoState) {
		_, err = fmt.Fprintln(request.output, "No Zalki state exists; nothing was moved. Reinstall Zalki and start new chats.")
		return err
	}
	if err != nil {
		return err
	}
	if err := plan.checkOffline(ctx, ops); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(request.output, "Source (%s): %q\nArchive: %q\n%s\nThis moves history, registrations, plugin data and settings inside this state directory. Nothing is deleted. After reinstalling, start new chats; old chats cannot read the new registrations.\n%s", request.origin, plan.source, plan.destination, archiveOfflineWarning, archiveQuestion); err != nil {
		return err
	}
	yes, err := request.confirm()
	if err != nil || !yes {
		return err
	}
	if err := plan.move(ctx, ops); err != nil {
		return err
	}
	_, err = fmt.Fprintf(request.output, "State archived to %q. Reinstall Zalki and start new chats; the next Zalki start will use empty state.\n", plan.destination)
	return err
}

type stateArchivePlan struct {
	source, parent, container, destination, home string
	sourceInfo, parentInfo                      os.FileInfo
}

func prepareStateArchive(request archiveRequest, ops archiveOps) (stateArchivePlan, error) {
	p := stateArchivePlan{source: request.source, home: request.home}
	info, err := os.Lstat(p.source)
	if errors.Is(err, os.ErrNotExist) {
		return p, errArchiveNoState
	}
	if err != nil {
		return p, err
	}
	if !info.IsDir() {
		return p, archiveRefusal("the selected state is not a real directory")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(p.source))
	if err != nil {
		return p, archiveRefusal("the state parent cannot be checked")
	}
	p.parent, p.source = parent, filepath.Join(parent, filepath.Base(p.source))
	if err := archiveProtectedRoot(p.source, request); err != nil {
		return p, err
	}
	if err := fsutil.CheckPrivateDir(p.source); err != nil {
		return p, archiveRefusal("the state is not a private directory owned by you")
	}
	if err := fsutil.CheckPrivateDir(p.parent); err != nil {
		return p, archiveRefusal("the state parent is not a private directory owned by you")
	}
	p.sourceInfo, p.parentInfo = info, nil
	p.parentInfo, err = os.Lstat(p.parent)
	if err != nil {
		return p, err
	}
	for _, name := range []string{"sessions", "messages"} {
		if err := fsutil.CheckPrivateDir(filepath.Join(p.source, name)); err != nil {
			return p, archiveRefusal("the selected folder is not a recognized Zalki state")
		}
	}
	p.container = filepath.Join(p.parent, "zalki.archive-"+ops.now().UTC().Format("20060102T150405Z")+"-"+ops.id())
	p.destination = filepath.Join(p.container, "state")
	return p, nil
}

func archiveProtectedRoot(source string, request archiveRequest) error {
	if !filepath.IsAbs(source) || source == string(filepath.Separator) || request.home == "" {
		return archiveRefusal("the selected state root is unsafe")
	}
	protected := []string{request.home, request.config}
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "XDG_CONFIG_HOME"} {
		if value := os.Getenv(name); filepath.IsAbs(value) {
			protected = append(protected, value)
		}
	}
	protected = append(protected, filepath.Join(request.home, ".codex"), filepath.Join(request.home, ".claude"), filepath.Join(request.home, ".cursor"), filepath.Join(request.home, ".config"))
	for _, path := range protected {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			resolved = filepath.Clean(path)
		}
		if source == resolved || strings.HasPrefix(resolved, source+string(filepath.Separator)) || (path != request.home && strings.HasPrefix(source, resolved+string(filepath.Separator))) {
			return archiveRefusal("the selection overlaps a home, configuration or native profile root")
		}
	}
	return nil
}

func (p stateArchivePlan) recheck() error {
	for path, expected := range map[string]os.FileInfo{p.source: p.sourceInfo, p.parent: p.parentInfo} {
		if err := fsutil.CheckPrivateDir(path); err != nil {
			return archiveRefusal("the state or its parent changed")
		}
		current, err := os.Lstat(path)
		if err != nil || !os.SameFile(current, expected) {
			return archiveRefusal("the state or its parent changed")
		}
	}
	return nil
}

func (p stateArchivePlan) checkOffline(ctx context.Context, ops archiveOps) error {
	if err := p.recheck(); err != nil {
		return err
	}
	held, err := p.holdActivity(ctx, ops)
	if held != nil {
		defer held.close()
	}
	return err
}

func (p stateArchivePlan) move(ctx context.Context, ops archiveOps) error {
	if err := p.recheck(); err != nil {
		return err
	}
	held, err := p.holdActivity(ctx, ops)
	if held != nil {
		defer held.close()
	}
	if err != nil {
		return err
	}
	if err := p.recheck(); err != nil {
		return err
	}
	if err := os.Mkdir(p.container, 0700); err != nil {
		return archiveRefusal("the new archive container could not be reserved exclusively")
	}
	if err := fsutil.CheckPrivateDir(p.container); err != nil {
		return archiveRefusal("the archive container is unsafe")
	}
	if _, err := os.Lstat(p.destination); !errors.Is(err, os.ErrNotExist) {
		return archiveRefusal("the archive payload path is not empty")
	}
	if err := held.linked(); err != nil {
		return err
	}
	if err := p.recheck(); err != nil {
		return err
	}
	if err := ops.rename(p.source, p.destination); err != nil {
		return p.renameFailure()
	}
	if err := errors.Join(ops.sync(p.container), ops.sync(p.parent)); err != nil {
		return fmt.Errorf("State moved to %q, but durability is unconfirmed. Check this archive before starting new chats; do not repeat the move without checking", p.destination)
	}
	return nil
}

func (p stateArchivePlan) renameFailure() error {
	source, sourceErr := os.Lstat(p.source)
	_, targetErr := os.Lstat(p.destination)
	if sourceErr == nil && os.SameFile(source, p.sourceInfo) && errors.Is(targetErr, os.ErrNotExist) {
		return archiveRefusal("the rename failed before a move was observed")
	}
	return fmt.Errorf("State move is unconfirmed between %q and %q. Check both paths before starting new chats; do not repeat the move without checking", p.source, p.destination)
}
