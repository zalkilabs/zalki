package bus

import (
	"errors"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
)

// Messages returns full history for local diagnostics and integrations.
func (s Store) Messages(actor, key string) ([]Message, error) {
	return s.QueryMessages(MessageQuery{Actor: actor, Capability: key, History: true, Limit: 100, IncludeBody: true})
}

type MessageQuery struct {
	Actor, Capability string
	History           bool
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
	actor, key, history, limit := query.Actor, query.Capability, query.History, query.Limit
	if limit == 0 {
		limit = 5
	}
	if limit < 1 || limit > 100 {
		return MessageBatch{}, fail(CodeInvalidInput, "limit must be 1..100")
	}
	batch := MessageBatch{Messages: []Message{}}
	items := []Message{}
	err := s.locked(func() error {
		current, err := s.authenticate(actor, key)
		if err != nil {
			return err
		}
		if query.MessageID != "" {
			message, err := s.message(query.MessageID)
			if err != nil {
				return err
			}
			if message.From != actor && message.To != actor {
				return fail(CodeUnauthorized, "message belongs to another session")
			}
			items = append(items, inboxView(message, current, false))
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
		files, err := filepath.Glob(filepath.Join(s.Dir, "messages", "*.json"))
		if err != nil {
			return err
		}
		for _, file := range files {
			var item Message
			item, err = s.message(strings.TrimSuffix(filepath.Base(file), ".json"))
			if err != nil {
				return err
			}
			if item.From == actor || item.To == actor {
				if !history && (item.To != actor || item.TargetGeneration != current.Generation || item.AcknowledgedAt != "" || !item.Status.canAcknowledge()) {
					continue
				}
				if cursor != nil && !messageComesBefore(*cursor, item, history) {
					continue
				}
				items = append(items, inboxView(item, current, query.History && !query.IncludeBody))
			}
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
		if current.Retired {
			return nil
		}
		files, err := filepath.Glob(filepath.Join(s.Dir, "messages", "*.json"))
		if err != nil {
			return err
		}
		pending := []Message{}
		for _, file := range files {
			var msg Message
			msg, err = s.message(strings.TrimSuffix(filepath.Base(file), ".json"))
			if err != nil {
				return err
			}
			if msg.To == actor && msg.TargetGeneration == current.Generation && hookPendingStatus(msg.Status) && msg.AcknowledgedAt == "" {
				pending = append(pending, msg)
			}
		}
		sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt < pending[j].CreatedAt })
		for i, msg := range pending {
			body := envelope(msg) + "\n\n"
			if i >= 5 || context.Len()+len(body) > 24*1024 {
				break
			}
			msg.Status = StatusContextEmitted
			msg.Receipt = &Receipt{At: timestamp(), Status: StatusContextEmitted}
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
