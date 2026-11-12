package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// A v2 child has two lanes. The feed lane is one offer at a time, answered by
// its ack. The request lane is one operation at a time, each with the child's
// request_id. The lanes interleave: an ack may arrive while an operation runs.
type v2Command struct {
	Ack     string     `json:"ack,omitempty"`
	Read    *v2Read    `json:"read,omitempty"`
	Next    *v2Next    `json:"inbox_next,omitempty"`
	Consume *v2Consume `json:"inbox_consume,omitempty"`
	Send    *v2Send    `json:"send,omitempty"`
}

type v2Read struct {
	RequestID string `json:"request_id"`
	Session   string `json:"session"`
	Message   string `json:"message"`
}

type v2Next struct {
	RequestID string `json:"request_id"`
}

type v2Consume struct {
	RequestID string `json:"request_id"`
	Message   string `json:"message"`
}

type v2Send struct {
	RequestID       string `json:"request_id"`
	ClientMessageID string `json:"client_message_id"`
	To              string `json:"to"`
	Body            string `json:"body"`
}

type v2Result struct {
	RequestID       string `json:"request_id"`
	Code            string `json:"code"`
	RetryAt         string `json:"retry_at,omitempty"`
	ClientMessageID string `json:"client_message_id,omitempty"`
	Status          string `json:"status,omitempty"`
	Message         any    `json:"message,omitempty"`
}

type v2Reply struct {
	ReadResult    *v2Result `json:"read_result,omitempty"`
	InboxResult   *v2Result `json:"inbox_result,omitempty"`
	InboxConsumed *v2Result `json:"inbox_consumed,omitempty"`
	SendResult    *v2Result `json:"send_result,omitempty"`
}

// v2EmptyPoll is the shortest time between two inbox_next calls that found
// nothing. A faster call gets rate_limited with retry_at.
const v2EmptyPoll = time.Second

// v2AckTimeout bounds an unanswered feed offer while no operation runs.
const v2AckTimeout = 5 * time.Second

type v2Host struct {
	ctx     context.Context
	feed    pluginfeed.PluginFeed
	service *bus.Service
	binding *pluginfeed.PluginFeedStatus
	child   *child
	acks    chan string
	ops     chan *v2Command
	failed  chan string
	done    chan struct{}
	busy    atomic.Bool
	offer   *pluginfeed.PluginDelivery
	offerAt time.Time
	emptyAt time.Time
}

func runV2Host(ctx context.Context, feed pluginfeed.PluginFeed, service *bus.Service, binding *pluginfeed.PluginFeedStatus, c *child, out io.Writer, ready string, header frame) error {
	if header.Hello == nil || header.Hello.Version != "v2" || header.ID != "" || header.Ack != "" || header.Gap != "" || header.Event != nil || c.reader.Buffered() != 0 {
		return errHelloVersion
	}
	c.readMode.Store(true)
	ctx, cancel := context.WithCancel(ctx)
	h := &v2Host{ctx: ctx, feed: feed, service: service, binding: binding, child: c, acks: make(chan string, 1), ops: make(chan *v2Command, 1), failed: make(chan string, 1), done: make(chan struct{})}
	go h.pump()
	defer func() { cancel(); c.cancel(); <-h.done }()
	if err := h.write(frame{Hello: &hello{Version: "v2"}}); err != nil {
		return err
	}
	return h.loop(out, ready)
}

func (h *v2Host) pump() {
	defer close(h.done)
	for h.ctx.Err() == nil {
		if code := bindingCode(h.feed, h.binding); code != "" {
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
		var f v2Command
		if err != nil || len(raw) > maxFrame || decodeFrame(raw, &f) != nil || !f.valid() || !h.queue(&f) {
			h.fail("protocol_error")
			return
		}
	}
}

// queue hands one frame to the loop. A second ack before the first is used,
// or a second operation before the first result, is a protocol error.
func (h *v2Host) queue(f *v2Command) bool {
	if f.Ack != "" {
		select {
		case h.acks <- f.Ack:
			return true
		default:
			return false
		}
	}
	if !h.busy.CompareAndSwap(false, true) {
		return false
	}
	h.ops <- f // capacity 1, and busy guarantees it is empty
	return true
}

func (h *v2Host) fail(code string) {
	select {
	case h.failed <- code:
	default:
	}
}

func (h *v2Host) loop(out io.Writer, ready string) error {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	announced := false
	for {
		select {
		case <-h.ctx.Done():
			return h.ctx.Err()
		case code := <-h.failed:
			return h.terminal(code)
		case id := <-h.acks:
			if err := h.ack(id); err != nil {
				return err
			}
		case f := <-h.ops:
			if err := h.handle(f); err != nil {
				return err
			}
		case <-tick.C:
			if err := h.tick(); err != nil {
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

func (h *v2Host) ack(id string) error {
	if h.offer == nil || id != h.offer.ID {
		return h.terminal("protocol_error")
	}
	if err := h.feed.AckPluginFeed(*h.offer); err != nil {
		return h.terminal(bindingCode(h.feed, h.binding))
	}
	h.offer = nil
	return nil
}

func (h *v2Host) tick() error {
	if h.offer != nil {
		if !h.busy.Load() && len(h.acks) == 0 && time.Since(h.offerAt) > v2AckTimeout {
			return h.terminal("protocol_error")
		}
		return nil
	}
	d, err := h.feed.NextPluginFeed(h.binding)
	if err != nil {
		return h.terminal(bindingCode(h.feed, h.binding))
	}
	if d.ID == "" {
		return nil
	}
	if err := h.write(frame{ID: d.ID, Gap: d.Gap, Event: d.Event}); err != nil {
		return err
	}
	h.offer, h.offerAt = &d, time.Now()
	return nil
}

func (h *v2Host) handle(f *v2Command) error {
	var reply v2Reply
	switch {
	case f.Read != nil:
		reply.ReadResult = h.read(f.Read)
	case f.Next != nil:
		reply.InboxResult = h.next(f.Next)
	case f.Consume != nil:
		reply.InboxConsumed = &v2Result{RequestID: f.Consume.RequestID, Code: h.feed.ConsumePluginInbox(h.binding, f.Consume.Message)}
		if reply.InboxConsumed.Code == "ok" {
			reply.InboxConsumed.Message = f.Consume.Message
		}
	default:
		reply.SendResult = h.sendMessage(f.Send)
	}
	if code := bindingCode(h.feed, h.binding); code != "" {
		return h.terminal(code)
	}
	// Free the lane before the child can see the result and send its next op.
	h.busy.Store(false)
	if h.offer != nil {
		h.offerAt = time.Now()
	}
	return h.write(reply)
}

func (h *v2Host) read(r *v2Read) *v2Result {
	raw, code, _ := h.feed.ReadPluginMessageBound(h.binding, r.Session, r.Message)
	result := &v2Result{RequestID: r.RequestID, Code: code}
	if code != "ok" && code != "native_deferred" {
		return result
	}
	var body struct {
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Message) == 0 {
		result.Code = "unavailable"
		return result
	}
	result.Message = body.Message
	return result
}

func (h *v2Host) next(r *v2Next) *v2Result {
	result := &v2Result{RequestID: r.RequestID}
	if retry := h.emptyAt.Add(v2EmptyPoll); !h.emptyAt.IsZero() && time.Now().Before(retry) {
		result.Code, result.RetryAt = "rate_limited", retry.UTC().Format(time.RFC3339Nano)
		return result
	}
	msg, code := h.feed.NextPluginInbox(h.binding)
	result.Code = code
	switch {
	case msg != nil: // "ok", or "too_large" without the body
		h.emptyAt = time.Time{}
		result.Message = msg
	case code == "ok":
		h.emptyAt = time.Now()
	}
	return result
}

func (h *v2Host) sendMessage(r *v2Send) *v2Result {
	result := &v2Result{RequestID: r.RequestID, ClientMessageID: r.ClientMessageID}
	m, err := h.service.SendFromPlugin(h.ctx, bus.PluginSendRequest{Installation: h.binding.ID, Name: h.binding.Name, ClientMessageID: r.ClientMessageID, To: r.To, Body: r.Body})
	result.Code = v2Code(err)
	if err == nil {
		result.Message, result.Status = m.ID, string(m.Status)
	}
	return result
}

// v2Code maps a core error to a result code. The text never reaches the child.
func v2Code(err error) string {
	if err == nil {
		return "ok"
	}
	switch model.ErrorCodeOf(err) {
	case model.CodeUnauthorized:
		return "unauthorized"
	case model.CodeConflict:
		return "conflict"
	case model.CodeInvalidInput, model.CodeInvalidIdentity:
		return "invalid_input"
	case model.CodeRateLimited:
		return "rate_limited"
	case model.CodeUnavailable, model.CodeNotFound, model.CodeUnsupportedSchema, model.CodeInvalidTransition, model.CodeInternal:
		return "unavailable"
	default:
		return "unavailable"
	}
}

func (h *v2Host) terminal(code string) error {
	if code == "" {
		code = "unavailable"
	}
	_ = h.write(map[string]string{"code": code})
	return h.failure()
}

func (h *v2Host) write(value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return h.failure()
	}
	raw = append(raw, '\n')
	if len(raw) > maxFrame || h.child.input.SetWriteDeadline(time.Now().Add(time.Second)) != nil {
		return h.failure()
	}
	n, err := h.child.input.Write(raw)
	if err != nil || n != len(raw) {
		return h.failure()
	}
	return nil
}

func (h *v2Host) failure() error {
	if err := h.ctx.Err(); err != nil {
		return err
	}
	return errors.New("plugin v2 failed")
}

// valid accepts exactly one ack or one operation with a token request_id.
func (f *v2Command) valid() bool {
	n, id := 0, ""
	if f.Ack != "" {
		n++
	}
	if f.Read != nil {
		n, id = n+1, f.Read.RequestID
	}
	if f.Next != nil {
		n, id = n+1, f.Next.RequestID
	}
	if f.Consume != nil {
		n, id = n+1, f.Consume.RequestID
	}
	if f.Send != nil {
		n, id = n+1, f.Send.RequestID
	}
	return n == 1 && (f.Ack != "" || validToken(id))
}

// validToken accepts 1-64 ASCII letters, digits, '_' or '-'.
func validToken(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
