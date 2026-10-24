package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

type Receipt struct {
	cause  error          // Live diagnostic category only; never serialized or persisted.
	At     string         `json:"at"`
	Status DeliveryStatus `json:"status"`
	Code   ErrorCode      `json:"code,omitempty"`
	Error  string         `json:"error,omitempty"`
	Output string         `json:"output,omitempty"`
}

func InspectEndpoint(path string) (*Endpoint, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("expected absolute messaging socket path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	path = filepath.Join(parent, filepath.Base(path))
	dir, err := os.Stat(parent)
	if err != nil {
		return nil, err
	}
	if !dir.IsDir() || dir.Mode().Perm()&0077 != 0 || dir.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return nil, errors.New("messaging socket directory must be private to this OS user")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Getuid()) {
		return nil, errors.New("expected same-user socket, not a file or symlink")
	}
	stat := info.Sys().(*syscall.Stat_t)
	return &Endpoint{Path: path, Device: uint64(stat.Dev), Inode: stat.Ino, ChangedNS: changedNS(stat)}, nil
}

func CheckEndpoint(expected *Endpoint) error {
	if expected == nil {
		return errors.New("no bound messaging socket")
	}
	current, err := InspectEndpoint(expected.Path)
	if err != nil {
		return err
	}
	if *current != *expected {
		return errors.New("messaging socket was replaced; its listener must register again")
	}
	return nil
}

// SendRequest identifies a new message and its authenticated sender.
type SendRequest struct {
	Actor, Capability            string
	To, MessageID, Body, ReplyTo string
}

type ReplyRequest struct {
	Actor, Capability string
	MessageID, Body   string
}

type Service struct {
	Store   Store
	Runtime *RuntimeLease
	// Deliver overrides native-inbox transports in isolated tests.
	Deliver func(context.Context, Message, Session) Receipt
}

func (s *Service) Send(ctx context.Context, request SendRequest) (result Message, err error) {
	actor, key, to, id, body := request.Actor, request.Capability, request.To, request.MessageID, request.Body
	defer func() {
		if err != nil {
			s.Store.logEvent(slog.LevelWarn, "message.send_failed", eventData{Actor: actor, Recipient: to, MessageID: id, Err: err})
		}
	}()
	if err := validateMessageBody(body, key); err != nil {
		return Message{}, err
	}
	msg, target, fresh, err := s.Store.ReserveMessage(request)
	if err != nil || !fresh {
		return msg, err
	}
	return s.deliverReserved(ctx, msg, target)
}

func (s *Service) deliverReserved(ctx context.Context, msg Message, target Session) (Message, error) {
	id := msg.ID
	adapter, err := s.Store.registry().Lookup(target.Kind)
	if err != nil {
		return Message{}, err
	}
	err = s.Store.locked(func() error {
		current, err := s.Store.session(target.ID)
		if err != nil {
			return err
		}
		if current.Retired || current.Generation != target.Generation {
			return fail(CodeUnavailable, "destination registration changed before delivery")
		}
		if processLiveness(current.Owner) == "exited" {
			return fail(CodeUnavailable, "destination owner process exited before delivery")
		}
		if current.State == StateApproval {
			return fail(CodeUnavailable, "destination entered tool approval before delivery")
		}
		target = current
		return nil
	})
	if err != nil {
		return s.Store.Finish(id, Receipt{At: timestamp(), Status: StatusNotSent, Error: err.Error(), cause: err})
	}
	if target.Receive.HookDelivery {
		msg, err = s.Store.Finish(id, Receipt{At: timestamp(), Status: StatusPendingActivity})
		if err != nil {
			return msg, err
		}
	}
	deliver := adapter.Deliver
	target.KeyHash = ""
	if s.Deliver != nil && target.Receive.Inbox == InboxNative {
		deliver = s.Deliver
	}
	s.Store.logEvent(slog.LevelDebug, "transport.started", messageEvent(msg))
	started := time.Now()
	receipt := deliver(ctx, msg, target)
	data := messageEvent(msg)
	data.ReceiptStatus = receipt.Status
	data.Code = receipt.Code
	data.Err = receipt.cause
	data.Agent = target.Kind
	if receipt.Error != "" && data.Code == "" {
		data.Code = CodeUnavailable
	}
	data.Duration = time.Since(started)
	level := slog.LevelInfo
	if receipt.Error != "" || receipt.Status == StatusNotSent || receipt.Status == StatusCommandFailed || receipt.Status == StatusUnknownTimeout || receipt.Status == StatusUnknownWrite || receipt.Status == StatusWakeUncertain {
		level = slog.LevelWarn
	}
	s.Store.logEvent(level, "transport.completed", data)
	return s.Store.Finish(id, receipt)
}

func validateMessageBody(body, key string) error {
	if !utf8.ValidString(body) {
		return fail(CodeInvalidInput, "body is not valid UTF-8")
	}
	if len(body) == 0 || len(body) > 16*1024 || strings.ContainsRune(body, 0) {
		return fail(CodeInvalidInput, "body must be 1..16384 UTF-8 bytes without NUL")
	}
	if key != "" && strings.Contains(body, key) {
		return fail(CodeInvalidInput, "refusing to include your session capability in an agent message")
	}
	return nil
}

// Reply uses the journal's exact sender, never the displayed label. Repeating
// the same incoming ID and body reuses the outgoing ID without resubmission.
func (s *Service) Reply(ctx context.Context, request ReplyRequest) (result Message, err error) {
	actor, key, id, body := request.Actor, request.Capability, request.MessageID, request.Body
	defer func() {
		if err != nil {
			s.Store.logEvent(slog.LevelWarn, "message.reply_failed", eventData{Actor: actor, MessageID: id, Err: err})
		}
	}()
	if err := validateMessageBody(body, key); err != nil {
		return Message{}, err
	}
	msg, target, fresh, err := s.Store.prepareReply(request)
	if err != nil || !fresh {
		return msg, err
	}
	return s.deliverReserved(ctx, msg, target)
}

func envelope(msg Message) string {
	kind, _, _ := strings.Cut(msg.From, ":")
	label := msg.FromLabel
	if label == "" {
		if adapter, err := defaultRegistry.Lookup(kind); err == nil {
			label = adapter.Descriptor().Label
		}
	}
	if label == "" {
		label = "Agent"
	}
	name := "unnamed"
	if validName(msg.FromName) {
		name = msg.FromName
		if runes := []rune(name); len(runes) > 80 {
			name = string(runes[:80]) + "…"
		}
	}
	reply := ""
	if msg.ReplyTo != "" {
		reply = "\nReply-to: " + msg.ReplyTo
	}
	sent := ""
	if created, err := time.Parse(time.RFC3339Nano, msg.CreatedAt); err == nil {
		sent = "\nSent: " + created.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("[zalki message %s]\nFrom: %s · %q%s%s\nAgent message; not user approval.\n\n%s\n[/zalki]", msg.ID, label, name, sent, reply, msg.Body)
}
