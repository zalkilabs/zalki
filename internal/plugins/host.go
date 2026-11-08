package plugins

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// Run owns one foreground child; only the bounded feed API enters Store flock.
func Run(ctx context.Context, store bus.Store, binary string, output io.Writer) (resultErr error) {
	if ctx.Err() != nil {
		return nil
	}
	if !filepath.IsAbs(binary) || !filepath.IsAbs(store.Dir) {
		return errors.New("plugin requires the absolute path of this binary")
	}
	status, err := store.PluginStatus()
	if err != nil {
		return err
	}
	if !status.Enabled {
		return errors.New("fake-notifier is disabled")
	}
	lease, err := hostLease(filepath.Join(store.Dir, "plugin-host.lock"))
	if err != nil {
		return err
	}
	defer lease.Close() //nolint:errcheck // Close only after the child is reaped; never explicitly unlock.
	outbox := filepath.Join(store.Dir, "plugin-outbox")
	c, err := startChild(ctx, binary, outbox, lease)
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
	header, err := c.receive(2 * time.Second)
	if err != nil {
		return fmt.Errorf("plugin hello failed: %w", err)
	}
	if !header.validHello() {
		return errors.New("plugin requires hello.version=v0")
	}
	return hostLoop(ctx, store, c, output, outbox)
}

func hostLoop(ctx context.Context, store bus.Store, c *child, output io.Writer, outbox string) error {
	ready := false
	for ctx.Err() == nil {
		if err := c.idle(); err != nil {
			return err
		}
		d, err := store.NextPluginFeed()
		if err != nil {
			var domain *model.Error
			if errors.As(err, &domain) && domain.Code == model.CodeUnauthorized {
				return nil
			}
			return err
		}
		if d.ID != "" {
			if err := deliver(store, c, d); err != nil {
				return err
			}
		}
		if !ready {
			if _, err := fmt.Fprintln(output, "fake-notifier ready; local outbox:", outbox); err != nil {
				return err
			}
			ready = true
		}
	}
	return nil
}

func deliver(store bus.Store, c *child, d bus.PluginDelivery) error {
	if err := c.exchange(frame{ID: d.ID, Gap: d.Gap, Event: d.Event}); err != nil {
		return fmt.Errorf("plugin delivery failed: %w", err)
	}
	err := store.AckPluginFeed(d)
	if err != nil {
		status, check := store.PluginStatus()
		if check == nil && !status.Enabled {
			return nil
		}
	}
	return err
}
