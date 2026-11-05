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
	limit := query.Limit
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
	err := s.readLocked(func() error {
		current, err := s.authenticate(query.Actor, query.Capability)
		if err != nil {
			return err
		}
		read := messageRead{query: query, current: current, history: query.History || query.Sent}
		if read.recipient, err = s.canonicalRecipient(query.To); err != nil {
			return err
		}
		if query.MessageID != "" {
			message, err := s.readExactMessage(read)
			if err == nil {
				batch.Messages = []Message{message}
			}
			return err
		}
		batch.Messages, batch.More, err = s.scanMessages(read, limit)
		return err
	})
	return batch, err
}

// messageRead is the authenticated view of one ReadMessages call, captured under
// the journal lock so exact lookup and scanning apply the same filters.
type messageRead struct {
	query     MessageQuery
	current   Session
	recipient string // canonical ID of the Sent filter, or empty
	history   bool
}

func (r messageRead) view(message Message, omitBody bool) Message {
	result := inboxView(message, r.current, omitBody)
	if r.query.Preview && !result.BodyDeferred {
		result.BodyPreview = bodyPreview(message.Body)
	}
	return result
}

// canonicalRecipient maps a name or ID to a session ID. Canonical IDs work even
// after the recipient's session was deleted.
func (s Store) canonicalRecipient(to string) (string, error) {
	if to == "" {
		return "", nil
	}
	if _, err := s.sessionPath(to); err == nil {
		return to, nil
	}
	target, err := s.resolve(to)
	if err != nil {
		return "", err
	}
	return target.ID, nil
}

func (s Store) readExactMessage(r messageRead) (Message, error) {
	actor := r.query.Actor
	message, err := s.message(r.query.MessageID)
	if err != nil {
		return Message{}, err
	}
	if message.From != actor && message.To != actor {
		return Message{}, fail(CodeUnauthorized, "message belongs to another session")
	}
	if r.query.Sent && message.From != actor {
		return Message{}, fail(CodeNotFound, "message was not sent by this session")
	}
	if r.recipient != "" && message.To != r.recipient {
		return Message{}, fail(CodeNotFound, "message was not sent to this recipient")
	}
	return r.view(message, false), nil
}

// scanMessages returns up to limit matching messages in display order and
// whether more remain. On failure it retains the views collected before the error.
func (s Store) scanMessages(r messageRead, limit int) (items []Message, more bool, err error) {
	items = []Message{}
	actor := r.query.Actor
	var cursor *Message
	if r.query.Cursor != "" {
		anchor, err := s.message(r.query.Cursor)
		if err != nil {
			return items, false, err
		}
		if anchor.From != actor && anchor.To != actor {
			return items, false, fail(CodeUnauthorized, "cursor belongs to another session")
		}
		cursor = &anchor
	}
	if err := s.eachMessage(func(_ string, item Message, readErr error) error {
		if readErr != nil {
			return readErr
		}
		if item.From != actor && item.To != actor {
			return nil
		}
		if r.query.Sent && item.From != actor {
			return nil
		}
		if r.recipient != "" && item.To != r.recipient {
			return nil
		}
		if !r.history && (item.To != actor || item.TargetGeneration != r.current.Generation || item.AcknowledgedAt != "" || !item.Status.canAcknowledge()) {
			return nil
		}
		if cursor != nil && !messageComesBefore(*cursor, item, r.history) {
			return nil
		}
		items = append(items, r.view(item, r.history && !r.query.IncludeBody))
		return nil
	}); err != nil {
		return items, false, err
	}
	sort.Slice(items, func(i, j int) bool { return messageComesBefore(items[i], items[j], r.history) })
	if len(items) > limit {
		return items[:limit], true, nil
	}
	return items, false, nil
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
