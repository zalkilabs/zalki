package bus

import (
	"context"
	"errors"
	"log/slog"
	"os"

	"github.com/zalkilabs/zalki/internal/model"
)

// PluginSendRequest is one send by an enabled plugin. The host fills
// Installation and Name from its checked binding, never from the child.
type PluginSendRequest struct {
	Installation, Name        string
	ClientMessageID, To, Body string
}

// pluginSendAdmit checks the send grant for one exact recipient. It runs under
// the journal lock before any session lookup, so a refusal reveals nothing.
type pluginSendAdmit func(view JournalView, stateDir, installation, to string) error

// pluginMessageID maps one client ID to one record forever. Messages are never
// deleted, so a retry with the same client ID can never send twice.
func pluginMessageID(installation, clientID string) string {
	return derivedID("zalki/plugin-send/v1", installation, clientID)
}

// SendFromPlugin stores a plugin message and delivers it on the native path:
// flood quota, approval hold and the recipient's own transport.
func (s *Service) SendFromPlugin(ctx context.Context, request PluginSendRequest) (result model.Message, err error) {
	defer func() {
		if err != nil {
			s.Store.logEvent(slog.LevelWarn, "message.send_failed", eventData{Actor: "plugin:" + request.Installation, Recipient: request.To, Err: err})
		}
	}()
	if !model.IsCanonicalUUID(request.Installation) || !model.ValidKind(request.Name) || !validClientMessageID(request.ClientMessageID) {
		return model.Message{}, model.NewError(model.CodeInvalidInput, "invalid plugin send request")
	}
	if err := model.ValidateMessageBody(request.Body, ""); err != nil {
		return model.Message{}, err
	}
	admit := s.PluginSend
	if admit == nil {
		return model.Message{}, model.NewError(model.CodeUnauthorized, "plugin send unavailable")
	}
	var msg model.Message
	var target model.Session
	var fresh bool
	err = WithJournalWrite(s.Store, func(view JournalView) error {
		if err := admit(view, s.Store.Dir, request.Installation, request.To); err != nil {
			return err
		}
		var err error
		msg, target, fresh, err = reservePluginMessage(s.Store, request)
		return err
	})
	if err != nil {
		return model.Message{}, err
	}
	if !fresh {
		return msg, nil
	}
	return s.deliverReserved(ctx, msg, target)
}

// reservePluginMessage runs under the journal lock after the grant check.
// fresh reports that the new message is ready for delivery now.
func reservePluginMessage(s Store, request PluginSendRequest) (message model.Message, target model.Session, fresh bool, err error) {
	from := "plugin:" + request.Installation
	id := pluginMessageID(request.Installation, request.ClientMessageID)
	old, err := s.message(id)
	if err == nil {
		if old.From != from || old.To != request.To || old.Body != request.Body || old.ReplyTo != "" || old.Kind != "" {
			return message, target, false, model.NewError(model.CodeConflict, "client message ID already used with different contents")
		}
		return old, target, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return message, target, false, err
	}
	if err := checkAdmissionBody(request.Body, s.MaxBodyBytes); err != nil {
		return message, target, false, err
	}
	if err := model.ValidateSessionAddress(request.To); err != nil {
		return message, target, false, err
	}
	target, err = s.session(request.To) // exact ID only; plugins never resolve names
	if errors.Is(err, os.ErrNotExist) {
		return message, target, false, model.NewError(model.CodeUnavailable, "recipient is not registered")
	}
	if err != nil {
		return message, target, false, err
	}
	if err := validateReservationTarget(s, target, from, ""); err != nil {
		return message, target, false, err
	}
	message = model.Message{Schema: recordSchema, ID: id, From: from, FromName: request.Name, FromLabel: model.PluginLabel, To: target.ID, Body: request.Body, TargetGeneration: target.Generation, CreatedAt: model.Timestamp(), Status: model.StatusAttempting}
	if err := prepareReservation(s, &message, target); err != nil {
		return message, target, false, err
	}
	if err := s.saveReservation(message); err != nil {
		return message, target, false, err
	}
	s.logReservation(message)
	return message, target, target.State != model.StateApproval, nil
}

// validClientMessageID accepts 1-64 ASCII letters, digits, '_' or '-'.
func validClientMessageID(id string) bool {
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
