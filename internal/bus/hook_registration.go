package bus

import (
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// Capture one immutable generation before any session-side operation. Only a
// trusted normalized prompt may create absence. Store.locked recovers an older
// WAL first, so its committed registration is never charged a second time.
func prepareHookRuntime(s Store, kind, actor string, event agent.HookInput) (hookRuntime, error) {
	h := hookRuntime{store: s, actor: actor, kind: kind, event: event.Event, generationBound: true}
	err := s.locked(func() error {
		v, err := s.session(actor)
		if errors.Is(err, os.ErrNotExist) && event.AllowPromptRegistration && event.Event == "UserPromptSubmit" {
			v, err = registerPromptAbsent(s, kind, actor, event, time.Now().UTC(), journal.WriteAtomic)
		}
		if errors.Is(err, os.ErrNotExist) {
			return errStaleHook
		}
		if err != nil {
			return err
		}
		native, err := canonicalNativeID(event.SessionID)
		if err != nil || v.NativeID != native {
			return model.NewError(model.CodeConflict, "native session identity collision")
		}
		h.generation = v.Generation
		return nil
	})
	return h, err
}

// The caller already proved ENOENT under the existing Store lock. Construction
// is shared with sessionStart. The durable quota debit precedes the unchanged
// session/event WAL; an unsuccessful registration may spend one attempt.
func registerPromptAbsent(s Store, kind, actor string, event agent.HookInput, now time.Time, write func(string, []byte) error) (model.Session, error) {
	v, _, err := newRegistration(s, kind, event.SessionID, event.CWD, nil, event)
	if err != nil {
		return v, err
	}
	if v.ID != actor {
		return v, model.NewError(model.CodeInvalidIdentity, "prompt registration identity mismatch")
	}
	if err := debitPromptRegistration(s.Dir, actor, now, write); err != nil {
		return v, err
	}
	if err := saveSessionFor(s, v, model.CauseRegister); err != nil {
		return v, err
	}
	s.logEvent(slog.LevelInfo, "session.registered", eventData{Actor: actor, Agent: kind, Generation: v.Generation, State: v.State})
	return v, nil
}

// Hook metadata is initialized before either native creation path publishes.
// Generic per-session policy belongs unconditionally in newRegistration.
func initializeHookRegistration(s Store, kind string, event agent.HookInput, v *model.Session) {
	if event.SessionTitle != nil && (*event.SessionTitle == "" || model.ValidName(*event.SessionTitle)) {
		v.Name = *event.SessionTitle
		if v.Name != "" {
			v.NameSource = kind + "_session_title_hook"
		}
	}
	s.updateOwner(v, kind, true)
	v.HookBuild = currentExecutable.loadedBuild
}
