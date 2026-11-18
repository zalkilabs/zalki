package pluginfeed

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const streamFrameLimit = 65536
const streamBatchLimit = 262144

type streamEnum struct{ Value, Meaning, Context string }

var streamGapEnums = []streamEnum{
	{"enabled", "Installation enabled at a new cut", "gap"},
	{"retention", "Journal history is no longer available", "gap"},
	{"epoch", "Journal identity changed", "gap"},
	{"launch", "A fresh child view requires a current cut", "gap"},
	{"policy", "Eligibility continuity cannot be proved", "gap"},
	{"membership", "The bounded observation window changed", "gap"},
	{"resource", "A bounded complete view is temporarily unavailable", "gap"},
	{"unavailable", "Current view cannot be proved from readable valid metadata", "gap"},
}

type streamFrame struct {
	ID    string       `json:"id,omitempty"`
	Gap   string       `json:"gap,omitempty"`
	Event *model.Event `json:"event,omitempty"`
}

type streamWireRow struct {
	ID         string             `json:"id"`
	Kind       string             `json:"kind"`
	Generation string             `json:"generation,omitempty"`
	State      model.SessionState `json:"state,omitempty"`
	Muted      bool               `json:"muted"`
	Name       string             `json:"name,omitempty"`
	Task       string             `json:"task,omitempty"`
}

func streamEnumContains(values []streamEnum, value string) bool {
	for _, entry := range values {
		if entry.Value == value {
			return true
		}
	}
	return false
}

func encodeStreamFrame(frame streamFrame) (json.RawMessage, error) {
	raw, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	return raw, validateStreamFrame(raw)
}

func validateStreamFrame(raw []byte) error {
	if len(raw)+1 > streamFrameLimit || !utf8.Valid(raw) {
		return journal.ErrInvalidFile
	}
	var frame streamFrame
	if err := decodeStreamJSON(raw, &frame); err != nil {
		return err
	}
	variants := 0
	for _, present := range []bool{frame.Gap != "", frame.Event != nil} {
		if present {
			variants++
		}
	}
	if variants != 1 {
		return journal.ErrInvalidFile
	}
	if !model.IsCanonicalUUID(frame.ID) {
		return journal.ErrInvalidFile
	}
	return validateStreamReceipt(frame)
}

func validateStreamReceipt(frame streamFrame) error {
	if frame.Gap != "" {
		if !streamEnumContains(streamGapEnums, frame.Gap) {
			return journal.ErrInvalidFile
		}
		return nil
	}
	return validateStreamEventWire(frame)
}

func validateStreamEventWire(frame streamFrame) error {
	event := *frame.Event
	if event.ID != frame.ID {
		return journal.ErrInvalidFile
	}
	// A projected state change may omit unobserved previous state. Validate
	// its remaining session fields using the same native field checks.
	if event.Type == model.EventSessionStateChanged && event.Session.Previous == "" {
		if event.Session.PreviousRetired {
			return journal.ErrInvalidFile
		}
		event.Type = model.EventSessionRegistered
	}
	if event.Message != nil {
		message := *event.Message
		event.Message = &message
		for _, address := range []*string{&message.From, &message.To} {
			kind, tail, ok := strings.Cut(*address, ":")
			if ok && tail == "" && model.ValidKind(kind) {
				*address = kind + ":00000000-0000-4000-8000-000000000001"
			}
		}
	}
	return event.Validate()
}

func validateStreamRow(raw []byte) error {
	if len(raw) > 4096 {
		return journal.ErrInvalidFile
	}
	var row streamWireRow
	if err := decodeStreamJSON(raw, &row); err != nil {
		return err
	}
	kind, _, _ := strings.Cut(row.ID, ":")
	if model.ValidateSessionAddress(row.ID) != nil || model.IsPluginInboxAddress(row.ID) || row.Kind != kind {
		return journal.ErrInvalidFile
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	return validateStreamRowFields(row, fields)
}

func validateStreamRowFields(row streamWireRow, fields map[string]json.RawMessage) error {
	if _, present := fields["muted"]; !present {
		return journal.ErrInvalidFile
	}
	if row.Muted {
		if len(fields) != 3 {
			return journal.ErrInvalidFile
		}
		return nil
	}
	if !model.IsCanonicalUUID(row.Generation) || !row.State.Valid() || row.State == model.StateRetired {
		return journal.ErrInvalidFile
	}
	for _, display := range []string{row.Name, row.Task} {
		if len(display) > 256 || (display != "" && !model.ValidName(display)) {
			return journal.ErrInvalidFile
		}
	}
	return nil
}

func validStreamPosition(position pluginPosition) bool {
	return (position.Epoch == "" && position.Sequence == 0) || model.IsCanonicalUUID(position.Epoch)
}
