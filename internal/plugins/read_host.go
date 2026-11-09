package plugins

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
)

type readRequest struct {
	Session string `json:"session"`
	Message string `json:"message"`
}
type readCommand struct {
	done chan struct{}
	Ack  string       `json:"ack,omitempty"`
	Read *readRequest `json:"read,omitempty"`
}
type readHost struct {
	ctx     context.Context
	feed    bus.PluginFeed
	binding *bus.PluginFeedStatus
	child   *child
	frames  chan *readCommand
	failed  chan string
	done    chan struct{}
	pending atomic.Pointer[readCommand]
	acked   bool
}

func runReadHost(ctx context.Context, feed bus.PluginFeed, binding *bus.PluginFeedStatus, c *child, out io.Writer, ready string, header frame) error {
	if header.Hello == nil || header.Hello.Version != "v1-read" || header.ID != "" || header.Ack != "" || header.Gap != "" || header.Event != nil || c.reader.Buffered() != 0 {
		return errors.New("plugin requires hello.version=v0")
	}
	c.readMode.Store(true)
	ctx, cancel := context.WithCancel(ctx)
	h := &readHost{ctx: ctx, feed: feed, binding: binding, child: c, frames: make(chan *readCommand), failed: make(chan string, 1), done: make(chan struct{})}
	c.reader = bufio.NewReaderSize(c.output, 4097)
	go h.pump()
	defer func() { cancel(); c.cancel(); <-h.done }()
	if err := h.send(frame{Hello: &hello{Version: "v1-read"}}); err != nil {
		return err
	}
	return h.loop(out, ready)
}

func (h *readHost) pump() {
	defer close(h.done)
	for h.ctx.Err() == nil {
		if code := h.bindingCode(); code != "" {
			h.fail(code)
			return
		}
		if h.child.output.SetReadDeadline(time.Now().Add(250*time.Millisecond)) != nil {
			h.fail("unavailable")
			return
		}
		raw, err := h.child.reader.ReadSlice('\n')
		if h.ctx.Err() != nil {
			return
		}
		if len(raw) == 0 && errors.Is(err, os.ErrDeadlineExceeded) {
			continue
		}
		var f readCommand
		if err != nil || len(raw) > 4096 || decodeFrame(raw, &f) != nil || (f.Read == nil) == (f.Ack == "") {
			h.fail("protocol_error")
			return
		}
		if !h.accept(&f) {
			h.fail("protocol_error")
			return
		}

		select {
		case h.frames <- &f:
		case <-h.ctx.Done():
			return
		}
	}
}

func (h *readHost) fail(code string) {
	select {
	case h.failed <- code:
	default:
	}
	if h.pending.Load() != nil {
		h.child.cancel()
	}
}

func (h *readHost) bindingCode() string {
	current, err := h.feed.PluginStatus()
	if err != nil || current.ID != h.binding.ID {
		return "stale_binding"
	}
	if !current.Enabled {
		return "revoked"
	}
	a, b := h.binding.Capabilities, current.Capabilities
	if a == nil || b == nil || h.binding.Launch == nil || current.Launch == nil || a.Installation != h.binding.ID || b.Installation != current.ID || a.PinSHA256 != h.binding.Launch.PinSHA256 || b.PinSHA256 != a.PinSHA256 || b.PinSHA256 != current.Launch.PinSHA256 || b.Revision != a.Revision {
		return "stale_binding"
	}
	return ""
}

func (h *readHost) loop(out io.Writer, ready string) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	announced := false
	for {
		select {
		case <-h.ctx.Done():
			return h.ctx.Err()
		case code := <-h.failed:
			return h.terminal(code)
		case f := <-h.frames:
			if !h.acked || f.Read == nil {
				return h.terminal("protocol_error")
			}
			if err := h.respond(f); err != nil {
				return err
			}
		case <-tick.C:
			if err := h.offer(); err != nil {
				return err
			}
			if !announced {
				if _, err := fmt.Fprintln(out, ready); err != nil {
					return err
				}
				announced = true
			}
		}
	}
}

func (h *readHost) offer() error {
	d, err := h.feed.NextPluginFeed()
	if err != nil {
		return h.terminal(h.bindingCode())
	}
	if d.ID == "" {
		return nil
	}
	if err := h.send(frame{ID: d.ID, Gap: d.Gap, Event: d.Event}); err != nil {
		return err
	}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-h.ctx.Done():
		return h.ctx.Err()
	case code := <-h.failed:
		return h.terminal(code)
	case <-timer.C:
		return h.terminal("protocol_error")
	case f := <-h.frames:
		if f.Read != nil || f.Ack != d.ID {
			return h.terminal("protocol_error")
		}
	}
	if err := h.feed.AckPluginFeed(d); err != nil {
		return h.terminal(h.bindingCode())
	}
	h.acked = true
	return nil
}

func (h *readHost) respond(request *readCommand) error {
	raw, code, _ := h.feed.ReadPluginMessageBound(h.binding, request.Read.Session, request.Read.Message)
	select {
	case failed := <-h.failed:
		return h.terminal(failed)
	default:
	}
	if err := h.ctx.Err(); err != nil {
		return err
	}
	if err := h.write(raw); err != nil {
		return err
	}
	close(request.done)
	h.pending.CompareAndSwap(request, nil)
	if code == "revoked" || code == "stale_binding" {
		return h.failure()
	}
	return nil
}

func (h *readHost) terminal(code string) error {
	if code == "" {
		code = "unavailable"
	}
	_ = h.write([]byte("{\"code\":\"" + code + "\"}\n"))
	return h.failure()
}

func (h *readHost) send(f frame) error {
	raw, err := json.Marshal(f)
	if err != nil {
		return h.failure()
	}
	return h.write(append(raw, '\n'))
}

func (h *readHost) write(raw []byte) error {
	if len(raw) > maxFrame || h.child.input.SetWriteDeadline(time.Now().Add(time.Second)) != nil {
		return h.failure()
	}
	n, err := h.child.input.Write(raw)
	if err != nil || n != len(raw) {
		return h.failure()
	}
	return nil
}

func (h *readHost) accept(f *readCommand) bool {
	if f.Read != nil && h.child.reader.Buffered() != 0 {
		return false
	}
	if pending := h.pending.Load(); pending != nil {
		// A client may receive the final byte before Write returns to its caller.
		select {
		case <-pending.done:
		case <-h.ctx.Done():
			return false
		case <-time.After(250 * time.Millisecond):
			return false
		}
	}
	if f.Read != nil {
		f.done = make(chan struct{})
		h.pending.Store(f)
	}
	return true
}

func (h *readHost) failure() error {
	if err := h.ctx.Err(); err != nil {
		return err
	}
	return errors.New("plugin v1-read failed")
}
