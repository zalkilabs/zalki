package bus

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// submitPluginInbox never invokes an adapter or acknowledges a native message.
func submitPluginInbox(s Store, request SendRequest) (model.Message, error) {
	var message model.Message
	err := s.locked(func() error {
		var err error
		message, err = preparePluginInbox(s, request)
		return err
	})
	if err != nil {
		return model.Message{}, err
	}
	return message, nil
}

func preparePluginInbox(s Store, request SendRequest) (model.Message, error) {

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
	f, p, err := pluginInboxTarget(s, request.To)
	if err != nil {
		return model.Message{}, err
	}
	if !pluginInboxAdmissionAllowed(p, sender.ID) {
		return model.Message{}, model.NewError(model.CodeUnauthorized, "plugin inbox admission denied")
	}
	labels := []model.Session{sender}
	s.nativeNames(labels)
	adapter, err := s.registry().Lookup(sender.Kind)
	if err != nil {
		return model.Message{}, err
	}
	now := time.Now().UTC()
	at := now.Format(time.RFC3339Nano)
	message := model.Message{Schema: recordSchema, ID: id, From: sender.ID, FromName: labels[0].Name, FromLabel: adapter.Descriptor().Label, To: request.To, Body: request.Body, TargetGeneration: p.ID, CreatedAt: at, Status: model.StatusSubmitted, Receipt: &model.Receipt{At: at, Status: model.StatusSubmitted}}
	index, err := f.loadPluginInboxIndex(p.ID, now)
	if err != nil {
		return model.Message{}, err
	}
	if err = f.reservePluginInbox(index, message, sender.Generation, p.Capabilities.Revision, now); err != nil {
		return model.Message{}, err
	}
	if err := s.saveMessage(message); err != nil {
		return model.Message{}, err
	}
	return message, nil
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
