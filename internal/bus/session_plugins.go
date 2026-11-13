package bus

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

const sessionPluginBytes = 24 << 10
const sessionPluginNames = 64
const pluginPolicyUnavailable = "session plugin policy is unavailable; register this session again\nAfter upgrading to rc.8, restart existing sessions to include them in plugin access."

type sessionPluginPolicy struct {
	Mode       string                       `json:"mode"`
	Allow      []string                     `json:"allow,omitempty"`
	InvalidEnv bool                         `json:"invalid_env,omitempty"`
	Muted      map[string]sessionPluginMute `json:"muted,omitempty"`
	LastChange sessionPluginChange          `json:"last_change"`
}

type sessionPluginMute struct {
	Actor string `json:"actor"`
	At    string `json:"at"`
}

type sessionPluginChange struct {
	ID     string `json:"id"`
	Actor  string `json:"actor"`
	At     string `json:"at"`
	Plugin string `json:"plugin,omitempty"`
	Muted  *bool  `json:"muted,omitempty"`
}

func validSessionPluginName(name string) bool {
	return len(name) <= 32 && model.ValidKind(name)
}

func sessionPluginTime(at string) bool {
	parsed, err := time.Parse(time.RFC3339Nano, at)
	return err == nil && !parsed.IsZero() && strings.HasSuffix(at, "Z")
}

func sessionPluginAudit(actor string) sessionPluginChange {
	return sessionPluginChange{ID: newID(), Actor: actor, At: model.Timestamp()}
}

func initialSessionPluginPolicy(actor string) (json.RawMessage, error) {
	value, _ := os.LookupEnv("ZALKI_PLUGINS")
	policy := parseSessionPluginEnvironment(value)
	policy.LastChange = sessionPluginAudit(actor)
	return encodeSessionPluginPolicy(policy)
}

func parseSessionPluginEnvironment(value string) sessionPluginPolicy {
	p := sessionPluginPolicy{Mode: "owner"}
	if value == "" {
		return p
	}
	p.Mode = "off"
	if value == "off" {
		return p
	}
	p.InvalidEnv = true
	if len(value) > 4096 {
		return p
	}
	names := strings.Split(value, ",")
	if len(names) > sessionPluginNames {
		return p
	}
	for _, name := range names {
		if !validSessionPluginName(name) {
			return p
		}
	}
	slices.Sort(names)
	p.Mode, p.Allow, p.InvalidEnv = "allow", slices.Compact(names), false
	return p
}

func decodeSessionPluginPolicy(session model.Session) (sessionPluginPolicy, bool) {
	var p sessionPluginPolicy
	raw := session.PluginPolicy
	if len(raw) == 0 || len(raw) > sessionPluginBytes || !sessionPluginUniqueJSON(raw) || DecodeStrictRecordJSON(raw, &p) != nil {
		return p, false
	}
	return p, validSessionPluginPolicy(p, session.ID)
}

func validSessionPluginPolicy(p sessionPluginPolicy, actor string) bool {
	if !validPluginPolicyBase(p) || len(p.Muted) > sessionPluginNames {
		return false
	}
	for name, entry := range p.Muted {
		if !validSessionPluginName(name) || entry.Actor != actor || !sessionPluginTime(entry.At) {
			return false
		}
	}
	c := p.LastChange
	if !model.IsCanonicalUUID(c.ID) || c.ID == "00000000-0000-0000-0000-000000000000" || c.Actor != actor || !sessionPluginTime(c.At) {
		return false
	}
	if c.Plugin == "" {
		return c.Muted == nil
	}
	return validSessionPluginToggle(p, c)
}

func validSessionPluginToggle(p sessionPluginPolicy, c sessionPluginChange) bool {
	if !validSessionPluginName(c.Plugin) || c.Muted == nil {
		return false
	}
	entry, exists := p.Muted[c.Plugin]
	if *c.Muted {
		return exists && entry.Actor == c.Actor && entry.At == c.At
	}
	return !exists
}

func validPluginPolicyBase(p sessionPluginPolicy) bool {
	if p.InvalidEnv && p.Mode != "off" {
		return false
	}
	switch p.Mode {
	case "off", "owner":
		return p.Allow == nil
	case "allow":
		if len(p.Allow) == 0 || len(p.Allow) > sessionPluginNames {
			return false
		}
		for i, name := range p.Allow {
			if !validSessionPluginName(name) || i > 0 && p.Allow[i-1] >= name {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func encodeSessionPluginPolicy(p sessionPluginPolicy) (json.RawMessage, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if len(raw) > sessionPluginBytes {
		return nil, model.NewError(model.CodeInvalidInput, "session plugin policy exceeds 24 KiB")
	}
	return raw, nil
}

func sessionAllowsPlugin(session model.Session, name string) bool {
	p, ok := decodeSessionPluginPolicy(session)
	if !ok || !validSessionPluginName(name) || p.Mode == "off" {
		return false
	}
	if _, muted := p.Muted[name]; muted {
		return false
	}
	return p.Mode == "owner" || slices.Contains(p.Allow, name)
}
