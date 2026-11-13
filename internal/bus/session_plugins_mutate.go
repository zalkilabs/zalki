package bus

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"log/slog"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const pluginAuthenticationFailed = "session plugin authentication failed; use your own actor and current capability"
const pluginChangeUncertain = "session plugin change may have been published; retry the same command to reconcile"

type sessionPluginWriter struct {
	save func(Store, model.Session) error
	sync func(string) error
}

// SetSessionPlugin changes only the authenticated actor's mute overlay. It
// cannot widen the environment ceiling or any owner's permission.
func SetSessionPlugin(s Store, actor, capability, name string, muted bool) error {
	writer := sessionPluginWriter{save: func(store Store, v model.Session) error { return saveSessionFor(store, v, "") }, sync: journal.SyncDir}
	return setSessionPlugin(s, actor, capability, name, muted, writer)
}

func setSessionPlugin(s Store, actor, key, name string, muted bool, writer sessionPluginWriter) error {
	if !validSessionPluginName(name) {
		return model.NewError(model.CodeInvalidInput, "plugin name must match [a-z][a-z0-9-]{0,31}")
	}
	return s.locked(func() error {
		current, err := authenticateSessionPlugin(s, actor, key)
		if err != nil {
			return err
		}
		policy, valid := decodeSessionPluginPolicy(current)
		if !valid {
			return model.NewError(model.CodeUnavailable, pluginPolicyUnavailable)
		}
		_, exists := policy.Muted[name]
		if exists == muted {
			return syncSessionPluginRetry(s, current, writer.sync)
		}
		if muted && len(policy.Muted) >= sessionPluginNames {
			return model.NewError(model.CodeInvalidInput, "session plugin mute limit is 64; unmute a plugin before muting another")
		}
		changeSessionPlugin(&policy, actor, name, muted, model.Timestamp())
		current.PluginPolicy, err = encodeSessionPluginPolicy(policy)
		if err != nil {
			return err
		}
		if err = writer.save(s, current); err != nil {
			return reconcileSessionPluginFailure(s, actor)
		}
		s.logEvent(slog.LevelInfo, "session.plugin_policy_changed", eventData{Actor: actor, Agent: current.Kind})
		return nil
	})
}

func authenticateSessionPlugin(s Store, actor, key string) (model.Session, error) {
	current, err := s.session(actor)
	digest := sha256.Sum256([]byte(key))
	if err != nil || key == "" || model.ValidateSessionAddress(actor) != nil || model.IsPluginInboxAddress(actor) || subtle.ConstantTimeCompare([]byte(current.KeyHash), []byte(hex.EncodeToString(digest[:]))) != 1 {
		return model.Session{}, model.NewError(model.CodeUnauthorized, pluginAuthenticationFailed)
	}
	if current.Retired {
		return current, model.NewError(model.CodeUnavailable, "session is retired; registration is required")
	}
	if s.sessionLiveness(current) == model.LivenessExited {
		return current, model.NewError(model.CodeUnavailable, "registered owner process has exited; resume or register the session again")
	}
	return current, nil
}

func changeSessionPlugin(p *sessionPluginPolicy, actor, name string, muted bool, at string) {
	if muted {
		if p.Muted == nil {
			p.Muted = make(map[string]sessionPluginMute)
		}
		p.Muted[name] = sessionPluginMute{Actor: actor, At: at}
	} else {
		delete(p.Muted, name)
	}
	p.LastChange = sessionPluginAudit(actor)
	p.LastChange.At = at
	p.LastChange.Plugin, p.LastChange.Muted = name, &muted
}

func syncSessionPluginRetry(s Store, current model.Session, sync func(string) error) error {
	path, err := s.sessionPath(current.ID)
	if err == nil {
		err = sync(filepath.Dir(path))
	}
	if err != nil {
		return model.NewError(model.CodeUnavailable, pluginChangeUncertain)
	}
	return nil
}

func reconcileSessionPluginFailure(s Store, actor string) error {
	if err := recoverEventCommit(s); err == nil {
		// Reread current disk truth, never roll back to the attempted preimage.
		if _, err = s.session(actor); err != nil {
			return model.NewError(model.CodeUnavailable, pluginChangeUncertain)
		}
	}
	return model.NewError(model.CodeUnavailable, pluginChangeUncertain)
}
