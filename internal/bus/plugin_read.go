package bus

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type pluginReadMessage struct {
	ID   string  `json:"id"`
	From string  `json:"from"`
	To   string  `json:"to"`
	Kind string  `json:"kind,omitempty"`
	Body *string `json:"body,omitempty"`
}

type pluginReadResult struct {
	Request string             `json:"request_id"`
	Code    string             `json:"code"`
	Message *pluginReadMessage `json:"message,omitempty"`
}

func pluginReadFailure(outcome string) error {
	code := model.CodeUnavailable
	switch outcome {
	case "denied", "revoked":
		code = model.CodeUnauthorized
	case "stale_binding", "stale_target":
		code = model.CodeConflict
	case "too_large", "invalid_input":
		code = model.CodeInvalidInput
	case "rate_limited":
		code = model.CodeRateLimited
	}
	return model.NewError(code, "plugin read %s", outcome)
}

// ReadPluginMessage is an owner proxy: it verifies a fresh installation snapshot,
// not process liveness. The returned JSON is audited before any bytes escape.
func (s PluginFeed) ReadPluginMessage(session, id string) ([]byte, error) {
	if err := s.checkName(); err != nil {
		return nil, pluginReadFailure("invalid_input")
	}
	canonical, err := model.UUID(id)
	if err != nil || canonical != id || len(session)+len(id) > 4096 {
		return nil, pluginReadFailure("invalid_input")
	}
	if err := s.checkCapabilityGrants(PluginCapabilityGrants{Read: []string{session}}); err != nil {
		return nil, pluginReadFailure("invalid_input")
	}
	before, err := s.PluginStatus()
	if err != nil {
		return nil, pluginReadFailure("unavailable")
	}
	if before.ID == "" || before.Launch == nil || before.Capabilities == nil {
		return nil, pluginReadFailure("denied")
	}
	if err := before.Launch.Verify(); err != nil {
		return nil, pluginReadFailure("stale_binding")
	}
	var result []byte
	var readErr error
	if err := s.locked(func() error {
		result, readErr = s.readPluginMessageAt(&before, session, id, time.Now().UTC())
		return nil
	}); err != nil {
		return nil, pluginReadFailure("unavailable")
	}
	return result, readErr
}

// Caller holds Store flock. The clock parameter also makes race/retention
// controls deterministic without changing the public operation or global time.
func (s PluginFeed) readPluginMessageAt(before *PluginFeedStatus, session, id string, now time.Time) ([]byte, error) {
	p, _, err := s.loadPlugin()
	if err != nil {
		return nil, pluginReadFailure("unavailable")
	}
	rows, err := s.loadPluginReadAudit(now)
	if err != nil {
		return nil, pluginReadFailure("audit_unavailable")
	}
	result := pluginReadResult{Request: newID(), Code: pluginReadAuthorization(p, before, session)}
	if result.Code == "ok" {
		for i := range rows {
			if rows[i].Plugin == before.ID && now.Sub(rows[i].At) < time.Second {
				return nil, pluginReadFailure("rate_limited")
			}
		}
		result.Message, result.Code = s.pluginInboxMessage(session, id)
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, pluginReadFailure("unavailable")
	}
	raw = append(raw, '\n')
	if len(raw) > 32<<10 {
		result.Code = "too_large"
		raw = nil
	}
	responseBytes := 0
	if result.Code == "ok" || result.Code == "native_deferred" {
		responseBytes = len(raw)
	}
	row := pluginReadAuditRow{At: now, Plugin: before.ID, Request: result.Request, Session: session, Message: id, Revision: before.Capabilities.Revision, Outcome: result.Code, Bytes: responseBytes}
	if err := s.savePluginReadAudit(append(rows, row), now); err != nil {
		return nil, pluginReadFailure("audit_unavailable")
	}
	if result.Code != "ok" && result.Code != "native_deferred" {
		return nil, pluginReadFailure(result.Code)
	}
	return raw, nil
}

func (s PluginFeed) pluginInboxMessage(session, id string) (*pluginReadMessage, string) {
	target, err := s.store.session(session)
	if err != nil || !canonicalEventID(target.Generation) {
		return nil, "unavailable"
	}
	raw, err := journal.ReadBytes(filepath.Join(s.store.Dir, "messages", id+".json"), maxStateFileBytes)
	var message model.Message
	if err != nil || decodeEventJSON(raw, &message) != nil || !validPluginReadMessage(&message, id) {
		return nil, "denied"
	}
	if message.To != session {
		return nil, "denied"
	}
	if message.TargetGeneration != target.Generation {
		return nil, "stale_target"
	}
	if message.Held || !message.Status.CanAcknowledge() {
		return nil, "unavailable"
	}
	if _, err := s.store.sessionRecordPath(message.From); err != nil {
		return nil, "unavailable"
	}
	view := inboxView(message, target, false)
	result := &pluginReadMessage{ID: message.ID, From: message.From, To: message.To, Kind: message.Kind}
	if view.BodyDeferred {
		return result, "native_deferred"
	}
	if len(view.Body) > 16<<10 {
		return nil, "too_large"
	}
	result.Body = &view.Body
	return result, "ok"
}

func pluginReadAuthorization(p pluginRecord, before *PluginFeedStatus, session string) string {
	c := p.Capabilities
	switch {
	case p.ID != before.ID:
		return "stale_binding"
	case !p.Enabled:
		return "revoked"
	case p.Launch == nil || c == nil || c.Installation != p.ID || c.PinSHA256 != p.Launch.PinSHA256 || c.PinSHA256 != before.Launch.PinSHA256 || c.Revision != before.Capabilities.Revision:
		return "stale_binding"
	case !slices.Contains(c.Read, session):
		return "denied"
	default:
		return "ok"
	}
}

func validPluginReadMessage(message *model.Message, id string) bool {
	return message.Schema == recordSchema && message.ID == id && (message.Kind == "" || message.Kind == model.NoticeKind)
}
