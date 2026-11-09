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
	if !validPluginReadInput(s, session, id) {
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
	raw, _, err := s.ReadPluginMessageBound(&before, session, id)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// ReadPluginMessageBound uses a caller-owned checked launch; errors are body-free.
func (s PluginFeed) ReadPluginMessageBound(before *PluginFeedStatus, session, id string) ([]byte, string, error) {
	request := newID()
	var raw []byte
	code := "invalid_input"
	readErr := pluginReadFailure(code)
	if validPluginReadInput(s, session, id) {
		code = "denied"
		readErr = pluginReadFailure(code)
		if before != nil && before.ID != "" && before.Launch != nil && before.Capabilities != nil {
			if err := s.locked(func() error {
				raw, code, readErr = s.readPluginMessageAt(before, session, id, request, time.Now().UTC())
				return nil
			}); err != nil {
				code = "unavailable"
				readErr = pluginReadFailure(code)
			}
		}
	}
	if len(raw) == 0 {
		raw, _ = json.Marshal(pluginReadResult{Request: request, Code: code})
		raw = append(raw, '\n')
	}
	return raw, code, readErr
}

func validPluginReadInput(s PluginFeed, session, id string) bool {
	canonical, err := model.UUID(id)
	return err == nil && canonical == id && len(session)+len(id) <= 4096 && s.checkName() == nil && s.checkCapabilityGrants(PluginCapabilityGrants{Read: []string{session}}) == nil
}

// Caller holds Store flock. The clock parameter also makes race/retention
// controls deterministic without changing the public operation or global time.
func (s PluginFeed) readPluginMessageAt(before *PluginFeedStatus, session, id, request string, now time.Time) ([]byte, string, error) {
	p, _, err := s.loadPlugin()
	if err != nil {
		return nil, "unavailable", pluginReadFailure("unavailable")
	}
	rows, err := s.loadPluginReadAudit(now)
	if err != nil {
		return nil, "audit_unavailable", pluginReadFailure("audit_unavailable")
	}
	result := pluginReadResult{Request: request, Code: pluginReadAuthorization(p, before, session)}
	if result.Code == "ok" {
		for i := range rows {
			if rows[i].Plugin == before.ID && now.Sub(rows[i].At) < time.Second {
				return nil, "rate_limited", pluginReadFailure("rate_limited")
			}
		}
		result.Message, result.Code = s.pluginInboxMessage(session, id)
	}
	return finishPluginReadResult(before, session, id, result, rows, now, s.savePluginReadAudit)
}

// finishPluginReadResult audits the bounded result before returning any bytes.
func finishPluginReadResult(before *PluginFeedStatus, session, id string, result pluginReadResult, rows []pluginReadAuditRow, now time.Time, save func([]pluginReadAuditRow, time.Time) error) ([]byte, string, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return nil, "unavailable", pluginReadFailure("unavailable")
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
	if err := save(append(rows, row), now); err != nil {
		return nil, "audit_unavailable", pluginReadFailure("audit_unavailable")
	}
	if result.Code != "ok" && result.Code != "native_deferred" {
		return nil, result.Code, pluginReadFailure(result.Code)
	}
	return raw, result.Code, nil
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
