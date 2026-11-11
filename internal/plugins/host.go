package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// Run owns one foreground child; only the bounded feed API enters Store flock.
func Run(ctx context.Context, store bus.Store, binary, name string, output io.Writer) (resultErr error) {
	if ctx.Err() != nil {
		return nil
	}
	if !filepath.IsAbs(binary) || !filepath.IsAbs(store.Dir) {
		return errors.New("plugin requires the absolute path of this binary")
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
	launch, ready, err := prepareLaunch(status, binary)
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
	defer func() {
		cause := resultErr
		resultErr = c.stop(cause)
		if c.canceledStop(ctx, cause) {
			resultErr = nil
		}
	}()
	header, err := c.receive(helloTimeout)
	if err != nil {
		return fmt.Errorf("plugin hello failed: %w", err)
	}

	if !header.validHello() {
		return runReadHost(ctx, feed, &status, c, output, ready, header)
	}
	return hostLoop(ctx, feed, c, output, ready)
}

func hostLoop(ctx context.Context, feed pluginfeed.PluginFeed, c *child, output io.Writer, readyMessage string) error {
	ready := false
	for ctx.Err() == nil {
		if err := c.idle(); err != nil {
			return err
		}
		d, err := feed.NextPluginFeed()
		if err != nil {
			var domain *model.Error
			if errors.As(err, &domain) && domain.Code == model.CodeUnauthorized {
				return nil
			}
			return err
		}
		if d.ID != "" {
			if err := deliver(feed, c, d); err != nil {
				return err
			}
		}
		if !ready {
			if _, err := fmt.Fprintln(output, readyMessage); err != nil {
				return err
			}
			ready = true
		}
	}
	return nil
}

func deliver(feed pluginfeed.PluginFeed, c *child, d pluginfeed.PluginDelivery) error {
	if err := c.exchange(frame{ID: d.ID, Gap: d.Gap, Event: d.Event}); err != nil {
		return fmt.Errorf("plugin delivery failed: %w", err)
	}
	err := feed.AckPluginFeed(d)
	if err != nil {
		status, check := feed.PluginStatus()
		if check == nil && !status.Enabled {
			return nil
		}
	}
	return err
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

func prepareLaunch(status pluginfeed.PluginFeedStatus, binary string) (pluginfeed.PluginLaunch, string, error) {
	ready := status.Name + " ready"
	if status.Name == "fake-notifier" {
		outbox := filepath.Join(status.DataDir, "outbox")
		return pluginfeed.PluginLaunch{Executable: binary, Args: []string{ChildCommand, "--outbox", outbox}}, ready + "; local outbox: " + outbox, nil
	}
	if status.Launch == nil {
		return pluginfeed.PluginLaunch{}, "", errors.New("plugin has no external executable")
	}
	return *status.Launch, ready, status.Launch.Verify()
}
