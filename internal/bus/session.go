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

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
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

// sessionRecordPath validates a canonical address without requiring its adapter.
// Cleanup can inspect the existence of a removed adapter's record safely.
func (s Store) sessionRecordPath(id string) (string, error) {
	parts := strings.Split(id, ":")
	if len(parts) != 2 || !model.ValidKind(parts[0]) {
		return "", fail(CodeInvalidIdentity, "use the complete kind:UUID returned by list_agents")
	}
	uuid, err := UUID(parts[1])
	if err != nil {
		return "", err
	}
	if uuid != parts[1] {
		return "", fail(CodeInvalidIdentity, "invalid session identity")
	}
	return filepath.Join(s.Dir, "sessions", strings.ReplaceAll(id, ":", "_")+".json"), nil
}

func (s Store) sessionPath(id string) (string, error) {
	path, err := s.sessionRecordPath(id)
	if err != nil {
		return "", err
	}
	kind, _, _ := strings.Cut(id, ":")
	if _, err := s.registry().Lookup(kind); err != nil {
		return "", err
	}
	return path, nil
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
	if v.Schema < 0 || v.Schema > leaseSessionSchema {
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
	if v.Listener != nil {
		v.Schema = leaseSessionSchema
	}
	if v.NativeID == "" {
		v.NativeID = NativeID(v)
	}
	if expected, err := s.sessionID(v.Kind, v.NativeID); err != nil || expected != v.ID {
		return fail(CodeInvalidIdentity, "native and Zalki session identities do not match")
	}
	var removed []string
	if migration, ok := adapter.(agent.SessionStateMigrationAdapter); ok {
		for _, name := range migration.LegacyStateFields() {
			if !coreField(name) {
				removed = append(removed, name)
			}
		}
	}
	return s.writeRecord(path, v, leaseSessionSchema, recordUpdate{removed: removed})
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
	session = Session{Schema: recordSchema, NativeID: native, ID: full, Kind: kind, CWD: cwd, Generation: newID(), State: StateReady, UpdatedAt: model.Timestamp(), Endpoint: endpoint, KeyHash: hex.EncodeToString(hash[:])}
	if names, ok := adapter.(agent.NameAdapter); ok {
		session.NativeConfigDir = names.ConfigDir()
	}
	session.Receive = adapter.ReceiveCapabilities(session)
	err = s.locked(func() error {
		preserved := false
		current, err := s.session(full)
		exists := err == nil
		if exists && NativeID(current) != native {
			return fail(CodeConflict, "native session identity collision")
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if compact && exists && !current.Retired {
			preserved = true
			session.Generation = current.Generation
			session.Name, session.NameSource = current.Name, current.NameSource
			session.Owner = current.Owner
			session.State = current.State
			session.AdapterState = current.AdapterState
			session.ApprovalHeld = current.ApprovalHeld
			session.Task = current.Task
		}
		if !preserved && exists {
			if err := s.settleApprovalQueue(full, "destination registration changed before delivery"); err != nil {
				return err
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
	if s.sessionLiveness(current) == LivenessExited {
		return current, fail(CodeUnavailable, "registered owner process has exited; resume or register the session again")
	}
	hash := sha256.Sum256([]byte(key))
	if subtle.ConstantTimeCompare([]byte(current.KeyHash), []byte(hex.EncodeToString(hash[:]))) != 1 {
		return current, fail(CodeUnauthorized, "wrong session capability; use your own SessionStart context")
	}
	return current, nil
}

func (s Store) Update(id string, state SessionState, retire bool) error {
	if !state.Valid() || (state == StateRetired) != retire {
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
		if retire {
			retireSession(&current, "session_end")
		} else {
			current.State, current.UpdatedAt = state, model.Timestamp()
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
func (s Store) sessionsWithWarnings() ([]Session, int, error) {
	sessions := []Session{}
	rows, issues, err := s.scanSessions()
	if err != nil {
		return nil, 0, err
	}
	for _, v := range rows {
		v.Liveness = s.sessionLiveness(v)
		if !v.Retired && v.Liveness != LivenessExited {
			v.KeyHash = ""
			sessions = append(sessions, v)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	s.nativeNames(sessions)
	for i := range sessions {
		adapter, err := s.registry().Lookup(sessions[i].Kind)
		if err != nil {
			return nil, 0, err
		}
		sessions[i].Receive = adapter.ReceiveCapabilities(sessions[i])
		sessions[i].NativeConfigDir = ""
		sessions[i].AdapterState = nil
		sessions[i].Listener = nil
		sessions[i].NativeID = ""
	}
	return sessions, len(issues), nil
}

func (s Store) sessions() ([]Session, error) {
	sessions, _, err := s.sessionsWithWarnings()
	return sessions, err
}

func (s Store) List() ([]Session, error) {
	sessions, _, err := s.ListWithWarnings()
	return sessions, err
}

// ListWithWarnings keeps the original row shape and reports skipped records once.
func (s Store) ListWithWarnings() ([]Session, int, error) {
	var sessions []Session
	var skipped int
	err := s.readLocked(func() error {
		var err error
		sessions, skipped, err = s.sessionsWithWarnings()
		return err
	})
	return sessions, skipped, err
}

// retireSession clears all active state, including any listener generation fence.
func retireSession(v *Session, reason string) {
	v.Retired, v.State, v.RetiredReason, v.Task, v.UpdatedAt = true, StateRetired, reason, "", model.Timestamp()
	releaseListenerLease(v)
}
