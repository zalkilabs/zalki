package bus

import (
	"os"
	"strings"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func (s Store) nativeNames(sessions []model.Session) {
	indexes := map[string]map[string]string{}
	for i := range sessions {
		v := &sessions[i]
		adapter, err := s.registry.Lookup(v.Kind)
		if err != nil {
			continue
		}
		names, ok := adapter.(agent.NameAdapter)
		if !ok {
			continue
		}
		name, source := names.LookupName(*v, indexes)
		if model.ValidName(name) {
			v.Name, v.NameSource = name, source
		}
	}
}

// resolve runs under the journal lock so ambiguity detection and reservation
// share a registry snapshot. Only registered sessions can be addressed by name.
func (s Store) resolve(destination string) (model.Session, error) {
	if _, err := s.sessionRecordPath(destination); err == nil {
		return s.session(destination) // Exact IDs always win over names.
	}
	kind, name := "", destination
	if prefix, rest, ok := strings.Cut(destination, ":"); ok {
		if _, err := s.registry.Lookup(prefix); err == nil {
			kind, name = prefix, rest
		}
	}
	if !model.ValidName(name) {
		return model.Session{}, model.NewError(model.CodeInvalidInput, "destination must be a complete kind:UUID or an exact native session name")
	}
	sessions, err := s.sessions()
	if err != nil {
		return model.Session{}, err
	}
	var matches []model.Session
	var ids []string
	for _, session := range sessions {
		if session.Name == name && (kind == "" || session.Kind == kind) {
			matches, ids = append(matches, session), append(ids, session.ID)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return model.Session{}, model.NewError(model.CodeConflict, "ambiguous session name %q; use an exact ID: %s", destination, strings.Join(ids, ", "))
	}
	if _, err := model.UUID(destination); err == nil {
		return model.Session{}, model.NewError(model.CodeUnavailable, "no registered session named %q; to needs a complete kind:UUID from list_agents or a session name, and a message ID is not an address: to answer a message use reply_message with its message_id", destination)
	}
	if startsAnID(destination, sessions) {
		return model.Session{}, model.NewError(model.CodeUnavailable, "no registered session named %q; it looks like the start of an ID, so use the complete kind:UUID from list_agents", destination)
	}
	return model.Session{}, model.NewError(model.CodeUnavailable, "no registered session named %q; use list_agents for names and exact IDs", destination)
}

// startsAnID reports whether text has the form kind:hex and is the start of a
// registered session ID. A plain name never matches. It only chooses the hint;
// it never resolves.
func startsAnID(text string, sessions []model.Session) bool {
	text = strings.ToLower(text)
	_, digits, ok := strings.Cut(text, ":")
	if !ok || len(digits) < 4 || strings.Trim(digits, "0123456789abcdef-") != "" {
		return false
	}
	for _, session := range sessions {
		if len(text) < len(session.ID) && strings.HasPrefix(session.ID, text) {
			return true
		}
	}
	return false
}

func (s Store) hookName(id, name string, fences ...sessionFence) error {
	if name != "" && !model.ValidName(name) {
		return nil
	}
	return s.locked(func() error {
		v, err := sessionUnderFence(s, id, fences...)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		v.Name, v.NameSource = name, v.Kind+"_session_title_hook"
		if name == "" {
			v.NameSource = ""
		}
		return s.saveSession(v)
	})
}
