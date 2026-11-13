package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// Run owns one foreground child; only the bounded feed API enters Store flock.
func Run(ctx context.Context, store bus.Store, name string, output io.Writer) (resultErr error) {
	if ctx.Err() != nil {
		return nil
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
	launch, ready, err := prepareLaunch(status)
	if err != nil {
		return err
	}
	c, err := launchChild(ctx, launch.Executable, launch.Args, status.DataDir, "", lease)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return nil
		}
		return err
	}
	// A plugin with message grants may see message text, so its stderr is
	// never shown, from the start: also when its hello fails or times out.
	// A feed-only plugin sees no text; its stderr is shown.
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

func prepareLaunch(status pluginfeed.PluginFeedStatus) (pluginfeed.PluginLaunch, string, error) {
	ready := status.Name + " ready"
	if status.Launch == nil {
		return pluginfeed.PluginLaunch{}, "", errors.New("plugin has no external executable")
	}
	return *status.Launch, ready, status.Launch.Verify()
}
