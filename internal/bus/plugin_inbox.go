package bus

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// pluginInboxAdmit selects an authorized plugin installation and returns its
// reservation step. Both calls run synchronously under the same journal lock.
type pluginInboxAdmit func(view JournalView, stateDir, to, sender, generation string) (func(model.Message, time.Time) error, error)

// submitPluginInbox never invokes an adapter or acknowledges a native message.
func submitPluginInbox(s Store, request SendRequest, admit pluginInboxAdmit) (model.Message, error) {
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

func preparePluginInbox(s Store, view JournalView, request SendRequest, admit pluginInboxAdmit) (model.Message, error) {
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
	if err := checkAdmissionBody(request.Body, s.MaxBodyBytes); err != nil {
		return model.Message{}, err
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

// AckPluginInboxMessage records that a plugin consumed one message of its own
// inbox, the same way a native recipient acknowledges. It needs the live write
// scope. A message that is already acknowledged is not an error.
func (v JournalView) AckPluginInboxMessage(stateDir, installation, id string) error {
	store, done, err := v.acquire(&stateDir)
	if err != nil {
		return model.NewError(model.CodeUnauthorized, "plugin write requires its live journal scope")
	}
	defer done()
	msg, _, err := currentMessage(store, id)
	if err != nil && !errors.Is(err, os.ErrNotExist) && !errors.Is(err, journal.ErrInvalidFile) {
		return err
	}
	if err != nil || msg.Kind != "" || msg.To != "plugin:"+installation || !msg.Status.CanAcknowledge() {
		return model.NewError(model.CodeNotFound, "plugin inbox message not found")
	}
	if msg.AcknowledgedAt != "" {
		return nil
	}
	msg.AcknowledgedAt = model.Timestamp()
	if err := store.saveMessage(msg); err != nil {
		return err
	}
	store.logEvent(slog.LevelInfo, "message.acknowledged", messageEvent(msg))
	return nil
}
