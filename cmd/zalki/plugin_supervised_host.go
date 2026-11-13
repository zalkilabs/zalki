package main

import (
	"context"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/plugins"
)

// lifelineGrace bounds a host after lifeline EOF: the plugin's 2-second
// EOF grace plus a margin; then the host ends its group, itself included.
const lifelineGrace = 4 * time.Second

// supervisedControlFD is the only descriptor a supervisor passes: its
// control pipe, always at 3.
const supervisedControlFD = 3

func newPluginRunCommand(opts *commandOptions) *cobra.Command {
	var controlFD int
	cmd := &cobra.Command{
		Use: "run NAME", Short: "Run one installed plugin in the foreground", Long: pluginTrustNotice + "\n\nPlugins MUST exit on stdin EOF within the 2-second grace period and must not daemonize. Helpers must exit with the plugin or not inherit FD 3. Interactive terminal input is unsupported. Plugins speak the one plugin wire: feed offers and ACKs, their own inbox, replies, and the extended send and read grants. The plugin runs as the owner; grants filter only what Zalki exposes.", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var control *os.File
			if cmd.Flags().Changed("supervised-control-fd") {
				var err error
				if control, err = openSupervisedControl(controlFD); err != nil {
					return err
				}
			}
			service, err := opts.service()
			if err != nil {
				return err
			}
			if control == nil {
				return plugins.Run(cmd.Context(), service.Store, args[0], cmd.OutOrStdout(), nil)
			}
			return runSupervisedHost(cmd.Context(), service.Store, args[0], control)
		},
	}
	cmd.Flags().IntVar(&controlFD, "supervised-control-fd", 0, "internal: the supervisor's control pipe")
	_ = cmd.Flags().MarkHidden("supervised-control-fd")
	return cmd
}

// runSupervisedHost runs one host for a supervisor. Its stdin is the
// supervisor's lifeline: EOF (a stop, or the supervisor's death) cancels the
// host. After that, as its last action, the host ends its own verified group,
// itself included, so no ordinary descendant outlives a dead supervisor.
func runSupervisedHost(ctx context.Context, store bus.Store, name string, control *os.File) error {
	return superviseLifeline(ctx, os.Stdin, func(ctx context.Context) error {
		return plugins.Run(ctx, store, name, io.Discard, control)
	})
}

// superviseLifeline runs run, canceling it on lifeline EOF. After EOF the
// end is bounded whatever run is doing (it may wait on a held journal
// lock): after lifelineGrace the whole group ends, this process included.
// When run returns, for any reason, the group ends at once as the last
// action (run has already sent its stopped frame): no ordinary descendant
// outlives its host, even if lifeline EOF has not been seen yet.
func superviseLifeline(ctx context.Context, lifelineInput io.Reader, run func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = io.Copy(io.Discard, lifelineInput)
		cancel()
		time.Sleep(lifelineGrace)
		_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
	}()
	err := run(ctx)
	_ = syscall.Kill(-os.Getpid(), syscall.SIGKILL)
	return err
}

// openSupervisedControl accepts only the supervisor's setup: FD 3 a
// write-only pipe, stdin a pipe (the lifeline), and this process the leader
// of its own group. FD 3 is closed on exec at once, so the plugin gets only
// its read-only lease there.
func openSupervisedControl(fd int) (*os.File, error) {
	refused := model.NewError(model.CodeInvalidInput, "supervised mode needs the supervisor's control pipe, its lifeline and its own process group")
	if fd != supervisedControlFD || !isPipe(fd) || !isPipe(0) || syscall.Getpgrp() != os.Getpid() {
		return nil, refused
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFL, 0)
	if err != nil || flags&unix.O_ACCMODE != unix.O_WRONLY {
		return nil, refused
	}
	syscall.CloseOnExec(fd)
	if err := syscall.SetNonblock(fd, true); err != nil {
		return nil, refused
	}
	return os.NewFile(uintptr(fd), "supervisor-control"), nil
}

func isPipe(fd int) bool {
	var stat unix.Stat_t
	return unix.Fstat(fd, &stat) == nil && stat.Mode&unix.S_IFMT == unix.S_IFIFO
}
