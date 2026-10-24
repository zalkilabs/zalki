package bus

import (
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Native titles are labels, not identities or instructions. Reject control
// characters rather than silently rewriting a name into a different address.
func validName(name string) bool {
	return name != "" && strings.TrimSpace(name) == name && len(name) <= 512 && utf8.ValidString(name) &&
		!strings.ContainsFunc(name, unicode.IsControl)
}

func configDir(variable, fallback string) string {
	if value := os.Getenv(variable); strings.TrimSpace(value) != "" {
		if path, err := filepath.Abs(value); err == nil {
			return path
		}
	}
	return fallback
}

func nativeID(v Session) string {
	if v.NativeID != "" {
		return v.NativeID
	}
	return strings.TrimPrefix(v.ID, v.Kind+":")
}

func (s Store) nativeNames(sessions []Session) {
	indexes := map[string]map[string]string{}
	for i := range sessions {
		v := &sessions[i]
		adapter, err := s.registry().Lookup(v.Kind)
		if err != nil {
			continue
		}
		names, ok := adapter.(nameAdapter)
		if !ok {
			continue
		}
		name, source := names.LookupName(*v, indexes)
		if validName(name) {
			v.Name, v.NameSource = name, source
		}
	}
}

func (s Store) rememberNativeConfigDir(id, kind string) error {
	adapter, err := s.registry().Lookup(kind)
	if err != nil {
		return err
	}
	names, ok := adapter.(nameAdapter)
	if !ok {
		return nil
	}
	root := names.ConfigDir()
	if root == "" {
		return nil
	}
	return s.locked(func() error {
		v, err := s.session(id)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if v.NativeConfigDir != "" {
			return nil
		}
		v.NativeConfigDir = root
		return s.saveSession(v)
	})
}

// resolve runs under the journal lock so ambiguity detection and reservation
// share a registry snapshot. Only registered sessions can be addressed by name.
func (s Store) resolve(destination string) (Session, error) {
	if _, err := s.sessionPath(destination); err == nil {
		return s.session(destination) // Exact IDs always win over names.
	}
	kind, name := "", destination
	if prefix, rest, ok := strings.Cut(destination, ":"); ok {
		if _, err := s.registry().Lookup(prefix); err == nil {
			kind, name = prefix, rest
		}
	}
	if !validName(name) {
		return Session{}, fail(CodeInvalidInput, "destination must be a complete kind:UUID or an exact native session name")
	}
	sessions, err := s.sessions()
	if err != nil {
		return Session{}, err
	}
	var matches []Session
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
		return Session{}, fail(CodeConflict, "ambiguous session name %q; use an exact ID: %s", destination, strings.Join(ids, ", "))
	}
	return Session{}, fail(CodeUnavailable, "no registered session named %q; use list_agents for names and exact IDs", destination)
}

func (s Store) hookName(id, name string) error {
	if name != "" && !validName(name) {
		return nil
	}
	return s.locked(func() error {
		v, err := s.session(id)
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
