package pluginfeed

import (
	"encoding/json"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

type pluginReadMessage struct {
	ID                string  `json:"id"`
	From              string  `json:"from"`
	To                string  `json:"to"`
	Kind              string  `json:"kind,omitempty"`
	Body              *string `json:"body,omitempty"`
	SenderGeneration  string  `json:"sender_generation,omitempty"`
	AdmissionRevision string  `json:"admission_revision,omitempty"`
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
	if before.ID == "" || before.Launch == nil || len(before.Access.Read) == 0 {
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
	request := model.NewUUID()
	var raw []byte
	code := "invalid_input"
	readErr := pluginReadFailure(code)
	if validPluginReadInput(s, session, id) {
		code = "denied"
		readErr = pluginReadFailure(code)
		if before != nil && before.ID != "" && before.Launch != nil && len(before.Access.Read) > 0 {
			if err := s.write(func(view bus.JournalView) error {
				raw, code, readErr = s.readPluginMessageAt(view, before, session, id, request, time.Now().UTC())
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
	return err == nil && canonical == id && len(session)+len(id) <= 4096 && s.checkName() == nil && s.checkAccess(PluginAccess{Read: []string{session}}) == nil
}

// Caller holds Store flock. The clock parameter also makes race/retention
// controls deterministic without changing the public operation or global time.
func (s pluginFiles) readPluginMessageAt(view bus.JournalView, before *PluginFeedStatus, session, id, request string, now time.Time) ([]byte, string, error) {
	p, _, err := s.loadPlugin()
	if err != nil {
		return nil, "unavailable", pluginReadFailure("unavailable")
	}
	rows, err := s.loadPluginReadAudit(now)
	if err != nil {
		return nil, "audit_unavailable", pluginReadFailure("audit_unavailable")
	}
	result := pluginReadResult{Request: request, Code: pluginReadAuthorization(view, p, before, session)}
	// A terminal binding code (revoked, stale) still wins, so the child stops.
	// Otherwise the limit applies to granted AND refused reads and writes no
	// row, so a flood of refused reads cannot fill the audit.
	if result.Code == "ok" || result.Code == "denied" {
		for i := range rows {
			if rows[i].Plugin == before.ID && now.Sub(rows[i].At) < time.Second {
				return nil, "rate_limited", pluginReadFailure("rate_limited")
			}
		}
	}
	if result.Code == "ok" {
		result.Message, result.Code = pluginNativeReadMessage(view, p, session, id)
	}
	return finishPluginReadResult(before, session, id, result, rows, now, s.savePluginReadAudit)
}

// maxRefusedReadRows caps the refused-read rows of one plugin in the shared
// audit; a new refused row replaces that plugin's oldest refused rows. The
// global 1000-row and size retention of the audit file is unchanged.
const maxRefusedReadRows = 64

// capRefusedReadRows makes room for row: when row is refused, it drops the
// oldest refused rows of row's plugin until fewer than the maximum are left,
// so after the append that plugin has at most the maximum. This also brings
// an older, larger audit back under the cap. Other rows are kept.
func capRefusedReadRows(rows []pluginReadAuditRow, row pluginReadAuditRow) []pluginReadAuditRow {
	refused := func(r pluginReadAuditRow) bool {
		return r.Plugin == row.Plugin && r.Outcome != "ok" && r.Outcome != "native_deferred"
	}
	if !refused(row) {
		return rows
	}
	excess := -(maxRefusedReadRows - 1)
	for _, r := range rows {
		if refused(r) {
			excess++
		}
	}
	kept := make([]pluginReadAuditRow, 0, len(rows))
	for _, r := range rows {
		if excess > 0 && refused(r) {
			excess--
			continue
		}
		kept = append(kept, r)
	}
	return kept
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
	row := pluginReadAuditRow{At: now, Plugin: before.ID, Request: result.Request, Session: session, Message: id, Revision: before.Revision, Outcome: result.Code, Bytes: responseBytes}
	if err := save(append(capRefusedReadRows(rows, row), row), now); err != nil {
		return nil, "audit_unavailable", pluginReadFailure("audit_unavailable")
	}
	if result.Code != "ok" && result.Code != "native_deferred" {
		return nil, result.Code, pluginReadFailure(result.Code)
	}
	return raw, result.Code, nil
}

func pluginNativeReadMessage(view bus.JournalView, p pluginRecord, session, id string) (*pluginReadMessage, string) {
	message, outcome := view.NativeInboxMessage(session, id)
	if outcome != "ok" && outcome != "native_deferred" {
		return nil, outcome
	}
	member, err := view.PluginMember(message.From, p.Name)
	if err != nil || !member {
		return nil, "denied"
	}
	result := &pluginReadMessage{ID: message.ID, From: message.From, To: message.To, Kind: message.Kind}
	if outcome == "ok" {
		result.Body = &message.Body
	}
	return result, outcome
}

// pluginReadAuthorization: the launch still holds, and session is in the
// exact read grant and a member now (live). Plugin and owner reads both use it.
func pluginReadAuthorization(view bus.JournalView, p pluginRecord, before *PluginFeedStatus, session string) string {
	if code := launchBindingCode(p, before); code != "ok" {
		return code
	}
	if !extendedMember(view, p, p.Access.Read, session) {
		return "denied"
	}
	return "ok"
}
