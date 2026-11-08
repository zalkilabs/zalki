package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
)

// CheckTarget selects a pinned executable, or the built-in Binary when Launch is nil.
// The caller retains ownership of an optional installed-name Lease until Check returns.
type CheckTarget struct {
	Launch *bus.PluginLaunch
	Binary string
	Lease  *os.File
}

// Check observes v0 transport and direct-child shutdown in temporary HOME/cwd/state.
// It does not grant access, advance real feeds, or prove arbitrary side effects durable.
func Check(ctx context.Context, target CheckTarget, output io.Writer) (result error) {
	root, err := os.MkdirTemp("", "zalki-plugin-check-")
	if err != nil {
		return reportCheck(output, "setup", err)
	}
	defer os.RemoveAll(root) //nolint:errcheck // Only the checker-owned temporary tree.
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return reportCheck(output, "setup", err)
	}
	home, cwd, state := filepath.Join(root, "home"), filepath.Join(root, "data"), filepath.Join(root, "state")
	for _, dir := range []string{home, cwd, state} {
		if err := os.Mkdir(dir, 0700); err != nil {
			return reportCheck(output, "setup", err)
		}
	}
	lease := target.Lease
	if lease == nil {
		lease, err = journal.OpenPluginLease(filepath.Join(state, "lease"))
		if err != nil {
			return reportCheck(output, "setup", err)
		}
		defer lease.Close() //nolint:errcheck // Child is reaped by the later defer first.
	}
	launch, err := checkLaunch(target, cwd)
	if err != nil {
		return reportCheck(output, "launch", err)
	}
	c, err := launchChild(ctx, launch.Executable, launch.Args, cwd, home, lease)
	if err != nil {
		return reportCheck(output, "launch", err)
	}
	defer finishCheck(c, output, &result)
	if err := reportCheck(output, "launch", nil); err != nil {
		return err
	}
	header, err := c.receive(2 * time.Second)
	if err == nil && !header.validHello() {
		err = errors.New("plugin requires hello.version=v0")
	}
	if err := reportCheck(output, "hello", err); err != nil {
		return err
	}
	offers, err := checkOffers()
	if err != nil {
		return reportCheck(output, "fixtures", err)
	}
	return checkExchanges(c, offers, output)
}

func checkLaunch(target CheckTarget, cwd string) (bus.PluginLaunch, error) {
	if target.Launch == nil {
		return bus.NewPluginLaunch(target.Binary, []string{ChildCommand, "--outbox", filepath.Join(cwd, "outbox")})
	}
	return *target.Launch, target.Launch.Verify()
}

func reportCheck(output io.Writer, stage string, err error) error {
	status := "PASS"
	if err != nil {
		status = "FAIL"
	}
	_, written := fmt.Fprintln(output, status, stage)
	if err != nil {
		err = fmt.Errorf("%s: %w", stage, err)
	}
	return errors.Join(err, written)
}

func finishCheck(c *child, output io.Writer, result *error) {
	stopped := c.stop(nil)
	if *result == nil {
		*result = reportCheck(output, "stdin-eof", stopped)
	} else {
		*result = errors.Join(*result, stopped)
	}
	*result = errors.Join(*result, reportCheck(output, "reaped", nil))
}

func checkExchanges(c *child, offers []frame, output io.Writer) error {
	for i, offer := range offers {
		if err := c.idle(); err != nil {
			return reportCheck(output, "stream", err)
		}
		if err := reportCheck(output, []string{"enabled-gap", "event-1", "redelivery", "event-2"}[i], c.exchange(offer)); err != nil {
			return err
		}
	}
	return nil
}
