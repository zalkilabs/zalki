package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// Run owns one foreground child; only the bounded feed API enters Store flock.
// With a supervisedControl pipe, Run reports ready and stopped frames on it
// instead of printing, and the plugin joins the host's process group, which
// the supervisor owns. nil keeps the manual foreground behavior.
func Run(ctx context.Context, store bus.Store, name string, output io.Writer, supervisedControl *os.File) (resultErr error) {
	if ctx.Err() != nil {
		return nil
	}
	var sup *supervision
	if supervisedControl != nil {
		sup = &supervision{file: supervisedControl}
		var stop func()
		output, stop = sup.readyPipe()
		defer func() {
			stop() // the ready frame, if any, is written before the stopped one
			_ = sup.send("stopped", sup.stopCode(resultErr))
		}()
	}
	if !filepath.IsAbs(store.Dir) {
		return errors.New("plugin requires an absolute state directory")
	}
	feed := pluginfeed.NewPluginFeed(store, name)
	status, err := feed.PluginStatus()
	if err != nil {
		return err
	}
	if !status.Enabled {
		return fmt.Errorf("%s is disabled", name)
	}
	lease, err := feed.AcquirePluginLease()
	if err != nil {
		return err
	}
	defer lease.Close() //nolint:errcheck // Close only after the child is reaped; never explicitly unlock.
	status, err = checkInstallation(feed, status.ID)
	if err != nil {
		return err
	}
	sup.bind(&status)
	launch, pinned, ready, err := sup.prepare(status)
	if err != nil {
		return err
	}
	c, err := launchChild(ctx, launch.Executable, pinned, launch.Args, status.DataDir, "", lease, sup == nil)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return nil
		}
		return err
	}
	// A plugin with message grants may see message text, so its stderr is
	// never shown, from the start: also when its hello fails or times out.
	// A feed-only plugin sees no text; its stderr is shown.
	sup.started(c.cmd.Process.Pid)
	c.readMode.Store(status.Access.Receive || len(status.Access.Send)+len(status.Access.Read) > 0)
	defer func() {
		cause := resultErr
		resultErr = c.stop(cause)
		if c.canceledStop(ctx, cause) {
			resultErr = nil
		}
	}()
	enabled, err := greet(c, grantedCapabilities(&status))
	if err != nil {
		return helloError(name, err)
	}
	service := &bus.Service{Store: store, PluginInbox: pluginfeed.AdmitPluginInbox, PluginSend: pluginfeed.BoundPluginSend(&status)}
	return runV2Host(ctx, feed, service, &status, c, output, ready, enabled)
}

func checkInstallation(feed pluginfeed.PluginFeed, id string) (pluginfeed.PluginFeedStatus, error) {
	current, err := feed.PluginStatus()
	if err != nil {
		return current, err
	}
	if !current.Enabled || current.ID != id {
		return current, errors.New("plugin installation changed before launch")
	}
	return current, nil
}

// prepareLaunch captures the executable's identity BEFORE the digest check,
// so a file swapped after the check is caught when it has started.
func prepareLaunch(status pluginfeed.PluginFeedStatus) (pluginfeed.PluginLaunch, fileIdentity, string, error) {
	ready := status.Name + " ready"
	if status.Launch == nil {
		return pluginfeed.PluginLaunch{}, fileIdentity{}, "", errors.New("plugin has no external executable")
	}
	pinned, err := pinnedIdentity(status.Launch.Executable)
	if err == nil {
		err = status.Launch.Verify()
	}
	if err != nil {
		return pluginfeed.PluginLaunch{}, fileIdentity{}, "", err
	}
	return *status.Launch, pinned, ready, nil
}
