package bus

import (
	"errors"
	"log/slog"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Messages returns full history for local diagnostics and integrations.
func (s Store) Messages(actor, key string) ([]Message, error) {
	return s.QueryMessages(MessageQuery{Actor: actor, Capability: key, History: true, Limit: 100, IncludeBody: true})
}

type MessageQuery struct {
	Actor, Capability string
	History           bool
	Sent              bool
	To                string
	Preview           bool
	Limit             int
	MessageID         string
	IncludeBody       bool
	Cursor            string
}

type MessageBatch struct {
	Messages []Message
	More     bool
}

// QueryMessages filters before applying the limit so read history cannot hide
// older unread work. Inbox order is oldest first; history is newest first.
func (s Store) QueryMessages(query MessageQuery) ([]Message, error) {
	batch, err := s.ReadMessages(query)
	return batch.Messages, err
}

// ReadMessages supports exact lookup without scanning the journal. History
// summaries omit bodies unless requested; canonical records are never changed.
func (s Store) ReadMessages(query MessageQuery) (MessageBatch, error) {
	actor, key, history, limit := query.Actor, query.Capability, query.History || query.Sent, query.Limit
	if limit == 0 {
		limit = 5
	}
	if limit < 1 || limit > 100 {
		return MessageBatch{}, fail(CodeInvalidInput, "limit must be 1..100")
	}
	if !query.Sent && (query.To != "" || query.Preview) {
		return MessageBatch{}, fail(CodeInvalidInput, "to and preview require sent:true")
	}
	batch := MessageBatch{Messages: []Message{}}
	items := []Message{}
	err := s.readLocked(func() error {
		current, err := s.authenticate(actor, key)
		if err != nil {
			return err
		}
		recipient := query.To
		if recipient != "" {
			// Canonical IDs work even after a recipient's session was deleted.
			if _, err := s.sessionPath(recipient); err != nil {
				target, err := s.resolve(recipient)
				if err != nil {
					return err
				}
				recipient = target.ID
			}
		}
		view := func(message Message, omitBody bool) Message {
			result := inboxView(message, current, omitBody)
			if query.Preview && !result.BodyDeferred {
				result.BodyPreview = bodyPreview(message.Body)
			}
			return result
		}
		if query.MessageID != "" {
			message, err := s.message(query.MessageID)
			if err != nil {
				return err
			}
			if message.From != actor && message.To != actor {
				return fail(CodeUnauthorized, "message belongs to another session")
			}
			if query.Sent && message.From != actor {
				return fail(CodeNotFound, "message was not sent by this session")
			}
			if recipient != "" && message.To != recipient {
				return fail(CodeNotFound, "message was not sent to this recipient")
			}
			items = append(items, view(message, false))
			return nil
		}
		var cursor *Message
		if query.Cursor != "" {
			anchor, err := s.message(query.Cursor)
			if err != nil {
				return err
			}
			if anchor.From != actor && anchor.To != actor {
				return fail(CodeUnauthorized, "cursor belongs to another session")
			}
			cursor = &anchor
		}
		if err := s.eachMessage(func(_ string, item Message, readErr error) error {
			if readErr != nil {
				return readErr
			}
			if item.From == actor || item.To == actor {
				if query.Sent && item.From != actor {
					return nil
				}
				if recipient != "" && item.To != recipient {
					return nil
				}
				if !history && (item.To != actor || item.TargetGeneration != current.Generation || item.AcknowledgedAt != "" || !item.Status.canAcknowledge()) {
					return nil
				}
				if cursor != nil && !messageComesBefore(*cursor, item, history) {
					return nil
				}
				items = append(items, view(item, history && !query.IncludeBody))
			}
			return nil
		}); err != nil {
			return err
		}
		sort.Slice(items, func(i, j int) bool { return messageComesBefore(items[i], items[j], history) })
		if len(items) > limit {
			batch.More = true
			items = items[:limit]
		}
		return nil
	})
	batch.Messages = items
	return batch, err
}

// bodyPreview produces one display line with a byte cap on UTF-8 boundaries.
func bodyPreview(body string) string {
	line := strings.Join(strings.Fields(body), " ")
	line = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, line)
	if len(line) > 80 {
		end := 80
		for !utf8.RuneStart(line[end]) {
			end--
		}
		line = line[:end]
	}
	return line
}

func messageComesBefore(a, b Message, history bool) bool {
	if a.CreatedAt == b.CreatedAt {
		return a.ID < b.ID
	}
	if history {
		return a.CreatedAt > b.CreatedAt
	}
	return a.CreatedAt < b.CreatedAt
}

func inboxView(message Message, current Session, omitBody bool) Message {
	if message.To == current.ID && current.Receive.Inbox == InboxNative && message.AcknowledgedAt == "" {
		message.Body, message.BodyDeferred = "", true
	} else if omitBody && message.Body != "" {
		message.Body, message.BodyOmitted = "", true
	}
	return message
}

// EmitPending is called by trusted lifecycle hooks for journal inbox adapters.
func (s Store) EmitPending(actor string) (string, error) {
	var context strings.Builder
	err := s.locked(func() error {
		current, err := s.session(actor)
		if err != nil {
			return err
		}
		if !current.Receive.HookDelivery {
			return errors.New("this adapter does not support hook delivery")
		}
		resolved, err := s.resolveApprovalQueue(current, true)
		if err != nil || resolved.unreachable {
			return err
		}
		pending := []Message{}
		if err := s.eachMessage(func(_ string, msg Message, readErr error) error {
			if readErr != nil {
				return readErr
			}
			if msg.To == actor && msg.TargetGeneration == current.Generation && hookPendingStatus(msg.Status) && msg.AcknowledgedAt == "" {
				pending = append(pending, msg)
			}
			return nil
		}); err != nil {
			return err
		}
		sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt < pending[j].CreatedAt })
		for i, msg := range pending {
			body := Envelope(s.withLabel(msg)) + "\n\n"
			if i >= 5 || context.Len()+len(body) > 24*1024 {
				break
			}
			if err := msg.emitContext(); err != nil {
				return err
			}
			if err := s.saveMessage(msg); err != nil {
				return err
			}
			s.logEvent(slog.LevelInfo, "message.context_emitted", messageEvent(msg))
			context.WriteString(body)
		}
		return nil
	})
	return context.String(), err
}
