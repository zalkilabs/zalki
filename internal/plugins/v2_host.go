package plugins

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// A child has two lanes. The feed lane is one offer at a time, answered by
// its ack. The request lane is one operation at a time, each in the envelope
// {op, request_id, idempotency_key, args} and answered by one reply with the
// same op and request_id. The lanes interleave: an ack may arrive while an
// operation runs.
type v2Command struct {
	Ack            string
	Op             string
	RequestID      string
	IdempotencyKey string
	Read           *v2Read
	Consume        *v2Consume
	Send           *v2Send
}

type v2Read struct {
	Session string `json:"session"`
	Message string `json:"message"`
}

type v2Consume struct {
	Message string `json:"message"`
}

type v2Send struct {
	// ReplyTo names the inbox message this send answers (the reply rule).
	ReplyTo string `json:"reply_to,omitempty"`
	To      string `json:"to"`
	Body    string `json:"body"`
}

type v2Result struct {
	Op        string         `json:"op"`
	RequestID string         `json:"request_id"`
	Code      string         `json:"code"`
	Result    map[string]any `json:"result,omitempty"`
}

type v2Reply struct {
	Reply *v2Result `json:"reply"`
}

// bindingCode compares the launch-time binding with the current record:
// "" while it still holds, else the terminal code for the child.
func bindingCode(feed pluginfeed.PluginFeed, binding *pluginfeed.PluginFeedStatus) string {
	if code := feed.PluginBindingCode(binding); code != "ok" {
		return code
	}
	return ""
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
	enabled []string
	// refusedOp holds the known operation (a string) the host refused: one
	// the hello did not enable. Set by the pump, read by the loop; never
	// plugin text.
	refusedOp atomic.Value
}

// runV2Host serves one child after its hello, with the enabled names.
func runV2Host(ctx context.Context, feed pluginfeed.PluginFeed, service *bus.Service, binding *pluginfeed.PluginFeedStatus, c *child, out io.Writer, ready string, enabled []string) error {
	if c.reader.Buffered() != 0 {
		return errHelloVersion // a frame before hello_ok: the plugin did not wait for it
	}
	ctx, cancel := context.WithCancel(ctx)
	h := &v2Host{ctx: ctx, feed: feed, service: service, binding: binding, child: c, acks: make(chan string, 1), ops: make(chan *v2Command, 1), failed: make(chan string, 1), done: make(chan struct{}), enabled: enabled}
	go h.pump()
	defer func() { cancel(); c.cancel(); <-h.done }()
	if err := h.loop(out, ready); !errors.Is(err, errDisabled) {
		return err
	}
	return nil
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
		var f frame
		if err != nil || len(raw) > maxFrame || decodeFrame(raw, &f) != nil {
			h.fail("protocol_error")
			return
		}
		cmd, ok := parseCommand(f)
		if ok && !h.allowed(cmd) {
			h.refusedOp.Store(cmd.Op) // parseCommand accepted only a known op name
		}
		if !ok || !h.allowed(cmd) || !h.queue(cmd) {
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
	if !slices.Contains(h.enabled, "feed") {
		return nil // the plugin did not ask for the feed
	}
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
	result := &v2Result{Op: f.Op, RequestID: f.RequestID}
	switch f.Op {
	case "read":
		h.read(f.Read, result)
	case "inbox_next":
		h.next(result)
	case "inbox_consume":
		result.Code = h.feed.ConsumePluginInbox(h.binding, f.Consume.Message)
		if result.Code == "ok" {
			result.Result = map[string]any{"message": f.Consume.Message}
		}
	default:
		h.sendMessage(f, result)
	}
	if code := bindingCode(h.feed, h.binding); code != "" {
		return h.terminal(code)
	}
	// Free the lane before the child can see the result and send its next op.
	h.busy.Store(false)
	if h.offer != nil {
		h.offerAt = time.Now()
	}
	return h.write(v2Reply{Reply: result})
}

func (h *v2Host) read(r *v2Read, result *v2Result) {
	raw, code, _ := h.feed.ReadPluginMessageBound(h.binding, r.Session, r.Message)
	result.Code = code
	if code != "ok" && code != "native_deferred" {
		return
	}
	var body struct {
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Message) == 0 {
		result.Code = "unavailable"
		return
	}
	result.Result = map[string]any{"message": body.Message}
}

func (h *v2Host) next(result *v2Result) {
	if retry := h.emptyAt.Add(v2EmptyPoll); !h.emptyAt.IsZero() && time.Now().Before(retry) {
		result.Code = "rate_limited"
		result.Result = map[string]any{"retry_at": retry.UTC().Format(time.RFC3339Nano)}
		return
	}
	msg, code := h.feed.NextPluginInbox(h.binding)
	result.Code = code
	switch {
	case msg != nil: // "ok", or "too_large" without the body
		h.emptyAt = time.Time{}
		result.Result = map[string]any{"message": msg}
	case code == "ok":
		h.emptyAt = time.Now()
	}
}

// sendMessage sends one message. The idempotency key is the client message
// ID: a retry with the same key gets the same message.
func (h *v2Host) sendMessage(f *v2Command, result *v2Result) {
	r := f.Send
	m, err := h.service.SendFromPlugin(h.ctx, bus.PluginSendRequest{Installation: h.binding.ID, Name: h.binding.Name, ClientMessageID: f.IdempotencyKey, ReplyTo: r.ReplyTo, To: r.To, Body: r.Body})
	result.Code = v2Code(err)
	if err == nil {
		result.Result = map[string]any{"message": m.ID, "status": string(m.Status)}
	}
}

// v2Code maps a core error to a result code. The text never reaches the child.
func v2Code(err error) string {
	if err == nil {
		return "ok"
	}
	if code := pluginfeed.PluginSendCode(err); code != "" {
		return code
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
	if code == "revoked" && h.ctx.Err() == nil {
		return errDisabled // the owner disabled it: a quiet, normal stop
	}
	if op, _ := h.refusedOp.Load().(string); op != "" && h.ctx.Err() == nil {
		// Host-made text from a fixed op name and code: shown even when
		// the child's stderr is withheld.
		name := h.binding.Name
		return hostHintError{fmt.Sprintf("%s stopped after Zalki refused %s (%s). Check access with: zalki plugins show %s", name, op, code, name)}
	}
	return h.failure()
}

// errDisabled ends the host loop when the owner disabled the plugin;
// runV2Host returns nil for it.
var errDisabled = errors.New("plugin disabled")

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

// parseCommand accepts exactly one ack, or one request in the envelope with
// a token request_id, an optional token idempotency_key (required for send)
// and the op's args. Unknown ops and unknown args fields are refused.
func parseCommand(f frame) (*v2Command, bool) {
	if f.Hello != nil || f.ID != "" || f.Gap != "" || f.Event != nil {
		return nil, false
	}
	if f.Ack != "" {
		return &v2Command{Ack: f.Ack}, !f.isRequest()
	}
	cmd := &v2Command{Op: f.Op, RequestID: f.RequestID, IdempotencyKey: f.IdempotencyKey}
	if !validToken(f.RequestID) || (f.IdempotencyKey != "" && !validToken(f.IdempotencyKey)) {
		return nil, false
	}
	target, ok := cmd.argsTarget(f)
	if !ok {
		return nil, false
	}
	if target == nil {
		return cmd, true // inbox_next without args
	}
	if f.Args == nil || decodeFrame(f.Args, target) != nil {
		return nil, false
	}
	return cmd, true
}

// argsTarget returns where the op's args decode to: nil for inbox_next
// without args, false for an unknown op or a send without a key.
func (cmd *v2Command) argsTarget(f frame) (any, bool) {
	switch f.Op {
	case "inbox_next":
		if f.Args == nil {
			return nil, true
		}
		return &struct{}{}, true
	case "inbox_consume":
		cmd.Consume = &v2Consume{}
		return cmd.Consume, true
	case "read":
		cmd.Read = &v2Read{}
		return cmd.Read, true
	case "send":
		cmd.Send = &v2Send{}
		return cmd.Send, f.IdempotencyKey != ""
	}
	return nil, false
}

// opCapability is the capability name a request needs.
func opCapability(cmd *v2Command) string {
	switch {
	case cmd.Op == "inbox_next" || cmd.Op == "inbox_consume":
		return "inbox"
	case cmd.Op == "read":
		return "read"
	case cmd.Send != nil && cmd.Send.ReplyTo != "":
		return "reply"
	default:
		return "send"
	}
}

// allowed refuses a request for a capability the hello did not enable.
func (h *v2Host) allowed(cmd *v2Command) bool {
	return cmd.Ack != "" || slices.Contains(h.enabled, opCapability(cmd))
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
