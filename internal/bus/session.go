package bus

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func (s Store) sessionID(kind, id string) (string, error) {
	if _, err := s.registry().Lookup(kind); err != nil {
		return "", err
	}
	native, err := canonicalNativeID(id)
	if err != nil {
		return "", err
	}
	if uuid, err := UUID(native); err == nil {
		return kind + ":" + uuid, nil
	}
	return kind + ":" + derivedID("zalki/native/v1", kind, native), nil
}

func (s Store) sessionPath(id string) (string, error) {
	parts := strings.Split(id, ":")
	if len(parts) != 2 {
		return "", fail(CodeInvalidIdentity, "use the complete kind:UUID returned by list_agents")
	}
	_, err := s.registry().Lookup(parts[0])
	uuid, uuidErr := UUID(parts[1])
	canonical := parts[0] + ":" + uuid
	if uuidErr != nil {
		return "", uuidErr
	}
	if err != nil || canonical != id {
		return "", fail(CodeInvalidIdentity, "invalid session identity")
	}
	return filepath.Join(s.Dir, "sessions", strings.ReplaceAll(id, ":", "_")+".json"), nil
}

func (s Store) session(id string) (Session, error) {
	var session Session
	path, err := s.sessionPath(id)
	if err == nil {
		session, err = s.readSession(path)
	}
	if err == nil && (session.ID != id || !strings.HasPrefix(id, session.Kind+":")) {
		err = fail(CodeInvalidIdentity, "session file identity mismatch")
	}
	if err == nil {
		adapter, adapterErr := s.registry().Lookup(session.Kind)
		if adapterErr != nil {
			return session, adapterErr
		}
		session.Receive = adapter.ReceiveCapabilities(session)
	}
	return session, err
}

func (s Store) saveSession(v Session) error {
	if v.Schema < 0 || v.Schema > recordSchema {
		return fail(CodeUnsupportedSchema, "unsupported session schema %d", v.Schema)
	}
	path, err := s.sessionPath(v.ID)
	if err != nil {
		return err
	}
	adapter, err := s.registry().Lookup(v.Kind)
	if err != nil {
		return err
	}
	v.Receive = adapter.ReceiveCapabilities(v)
	v.Schema = recordSchema
	if v.NativeID == "" {
		v.NativeID = NativeID(v)
	}
	if expected, err := s.sessionID(v.Kind, v.NativeID); err != nil || expected != v.ID {
		return fail(CodeInvalidIdentity, "native and Zalki session identities do not match")
	}
	return writeJSON(path, v)
}

func (s Store) Register(kind, id, cwd string, endpoint *Endpoint) (Session, string, error) {
	return s.register(kind, id, cwd, endpoint, false)
}

// Compaction refreshes the model's private capability without replacing the
// receiving registration. Its native queue still belongs to that registration.
// Startup/resume and explicit registration continue to create a new generation.
func (s Store) register(kind, id, cwd string, endpoint *Endpoint, compact bool) (Session, string, error) {
	var session Session
	full, err := s.sessionID(kind, id)
	if err != nil {
		return session, "", err
	}
	if !filepath.IsAbs(cwd) {
		return session, "", fail(CodeInvalidInput, "working directory must be absolute")
	}
	adapter, err := s.registry().Lookup(kind)
	if err != nil {
		return session, "", err
	}
	if adapter.ReceiveCapabilities(Session{}).RequiresEndpoint && endpoint == nil {
		return session, "", fmt.Errorf("%s registration requires its own socket", adapter.Descriptor().Label)
	}
	key := newID() + newID()
	hash := sha256.Sum256([]byte(key))
	native, _ := canonicalNativeID(id)
	session = Session{Schema: recordSchema, NativeID: native, ID: full, Kind: kind, CWD: cwd, Generation: newID(), State: StateReady, UpdatedAt: timestamp(), Endpoint: endpoint, KeyHash: hex.EncodeToString(hash[:])}
	if names, ok := adapter.(nameAdapter); ok {
		session.NativeConfigDir = names.ConfigDir()
	}
	session.Receive = adapter.ReceiveCapabilities(session)
	err = s.locked(func() error {
		preserved := false
		if current, err := s.session(full); err == nil && NativeID(current) != native {
			return fail(CodeConflict, "native session identity collision")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if compact {
			current, err := s.session(full)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err == nil && !current.Retired {
				preserved = true
				session.Generation = current.Generation
				session.Name, session.NameSource = current.Name, current.NameSource
				session.Owner = current.Owner
				session.State = current.State
				session.AdapterState = current.AdapterState
			}
		}
		if err := s.saveSession(session); err != nil {
			return err
		}
		s.logEvent(slog.LevelInfo, "session.registered", eventData{Actor: session.ID, Agent: kind, Generation: session.Generation, State: session.State, Reused: preserved})
		return nil
	})
	return session, key, err
}

func (s Store) authenticate(id, key string) (Session, error) {
	current, err := s.session(id)
	if err != nil {
		return current, err
	}
	if current.Retired {
		return current, fail(CodeUnavailable, "session is retired; registration is required")
	}
	if processLiveness(current.Owner) == "exited" {
		return current, fail(CodeUnavailable, "registered owner process has exited; resume or register the session again")
	}
	hash := sha256.Sum256([]byte(key))
	if subtle.ConstantTimeCompare([]byte(current.KeyHash), []byte(hex.EncodeToString(hash[:]))) != 1 {
		return current, fail(CodeUnauthorized, "wrong session capability; use your own SessionStart context")
	}
	return current, nil
}

func (s Store) Update(id string, state SessionState, retire bool) error {
	if !state.valid() || (state == StateRetired) != retire {
		return fail(CodeInvalidTransition, "invalid session state/retirement transition")
	}
	return s.locked(func() error {
		current, err := s.session(id)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Retired && !retire {
			return nil
		}
		previous := current.State
		current.State, current.UpdatedAt = state, timestamp()
		if retire {
			current.Retired = true
			current.RetiredReason = "session_end"
		}
		if err := s.saveSession(current); err != nil {
			return err
		}
		if previous != state {
			level := slog.LevelDebug
			if state == StateApproval || retire {
				level = slog.LevelInfo
			}
			s.logEvent(level, "session.state_changed", eventData{Actor: id, Generation: current.Generation, State: state})
		}
		return nil
	})
}

// sessions is called with the journal lock held. Native names are a read-only
// discovery view: they never change a session's routing identity or capability.
func (s Store) sessions() ([]Session, error) {
	sessions := []Session{}
	rows, err := s.rawSessions()
	if err != nil {
		return nil, err
	}
	for _, v := range rows {
		v.Liveness = processLiveness(v.Owner)
		if !v.Retired && v.Liveness != "exited" {
			v.KeyHash = ""
			sessions = append(sessions, v)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	s.nativeNames(sessions)
	for i := range sessions {
		adapter, err := s.registry().Lookup(sessions[i].Kind)
		if err != nil {
			return nil, err
		}
		sessions[i].Receive = adapter.ReceiveCapabilities(sessions[i])
		sessions[i].NativeConfigDir = ""
		sessions[i].AdapterState = nil
		sessions[i].NativeID = ""
	}
	return sessions, nil
}

func (s Store) List() ([]Session, error) {
	var sessions []Session
	err := s.locked(func() error {
		var err error
		sessions, err = s.sessions()
		return err
	})
	return sessions, err
}
