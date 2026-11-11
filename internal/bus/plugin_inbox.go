package bus

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// PluginInboxAdmit selects an authorized plugin installation and returns its
// reservation step. Both calls run synchronously under the same journal lock.
type PluginInboxAdmit func(view JournalView, stateDir, to, sender, generation string) (func(model.Message, time.Time) error, error)

// submitPluginInbox never invokes an adapter or acknowledges a native message.
func submitPluginInbox(s Store, request SendRequest, admit PluginInboxAdmit) (model.Message, error) {
	var message model.Message
	err := WithJournalWrite(s, func(view JournalView) error {
		var err error
		message, err = preparePluginInbox(s, view, request, admit)
		return err
	})
	if err != nil {
		return model.Message{}, err
	}
	return message, nil
}

func preparePluginInbox(s Store, view JournalView, request SendRequest, admit PluginInboxAdmit) (model.Message, error) {
	id, err := model.UUID(request.MessageID)
	if err != nil {
		return model.Message{}, err
	}
	sender, err := s.authenticate(request.Actor, request.Capability)
	if err != nil {
		return model.Message{}, err
	}
	if !canonicalEventID(sender.Generation) {
		return model.Message{}, model.NewError(model.CodeUnavailable, "native sender generation unavailable")
	}
	if request.ReplyTo != "" {
		return model.Message{}, model.NewError(model.CodeInvalidInput, "plugin inbox replies are unavailable")
	}
	request.MessageID = id
	old, found, err := existingPluginInboxMessage(s, request)
	if err != nil || found {
		return old, err
	}
	if admit == nil {
		return model.Message{}, model.NewError(model.CodeUnauthorized, "plugin inbox unavailable")
	}
	reserve, err := admit(view, s.Dir, request.To, sender.ID, sender.Generation)
	if err != nil {
		return model.Message{}, err
	}
	if reserve == nil {
		return model.Message{}, model.NewError(model.CodeUnauthorized, "plugin inbox unavailable")
	}
	labels := []model.Session{sender}
	s.nativeNames(labels)
	adapter, err := s.registry().Lookup(sender.Kind)
	if err != nil {
		return model.Message{}, err
	}
	now := time.Now().UTC()
	at := now.Format(time.RFC3339Nano)
	message := model.Message{Schema: recordSchema, ID: id, From: sender.ID, FromName: labels[0].Name, FromLabel: adapter.Descriptor().Label, To: request.To, Body: request.Body, TargetGeneration: strings.TrimPrefix(request.To, "plugin:"), CreatedAt: at, Status: model.StatusSubmitted, Receipt: &model.Receipt{At: at, Status: model.StatusSubmitted}}
	if err := reserve(message, now); err != nil {
		return model.Message{}, err
	}
	if err := s.saveMessage(message); err != nil {
		return model.Message{}, err
	}
	return message, nil
}

// AdmitPluginInbox is the CLI's local plugin inbox admission implementation.
// The returned operation is used once, before the journal callback returns.
func AdmitPluginInbox(view JournalView, stateDir, to, sender, generation string) (func(model.Message, time.Time) error, error) {
	var reserve func(model.Message, time.Time) error
	err := view.WithWriteScope(stateDir, func() error {
		f, p, err := pluginInboxTarget(stateDir, to)
		if err != nil {
			return err
		}
		if !pluginInboxAdmissionAllowed(p, sender) {
			return model.NewError(model.CodeUnauthorized, "plugin inbox admission denied")
		}
		reserve = func(message model.Message, now time.Time) error {
			return view.WithWriteScope(stateDir, func() error {
				index, err := f.loadPluginInboxIndex(view, p.ID, now)
				if err != nil {
					return err
				}
				return f.reservePluginInbox(index, message, generation, p.Capabilities.Revision, now)
			})
		}
		return nil
	})
	return reserve, err
}

func pluginInboxAdmissionAllowed(p pluginRecord, sender string) bool {
	c := p.Capabilities
	return p.Launch != nil && c != nil && c.Installation == p.ID && c.PinSHA256 == p.Launch.PinSHA256 && slices.Contains(c.InboxFrom, sender)
}

func existingPluginInboxMessage(store Store, request SendRequest) (model.Message, bool, error) {
	old, err := store.message(request.MessageID)
	if err == nil {
		if old.From != request.Actor || old.To != request.To || old.Body != request.Body || old.ReplyTo != "" {
			return model.Message{}, false, model.NewError(model.CodeConflict, "message UUID already used with different contents")
		}
		// Confirm durability after an earlier ambiguous post-rename failure.
		if err = journal.SyncDir(filepath.Join(store.Dir, "messages")); err != nil {
			return model.Message{}, false, err
		}
		return old, true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return model.Message{}, false, nil
	}
	return model.Message{}, false, err
}
