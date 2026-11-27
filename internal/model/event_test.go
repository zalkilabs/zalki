package model_test

import (
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func validMetadataEvent() model.Event {
	return model.Event{ID: "00000000-0000-4000-8000-000000000001",
		Type: "session.state_changed", OccurredAt: "2026-11-07T11:00:00.000000000Z",
		TransactionID: "00000000-0000-4000-8000-000000000002",
		Session: model.EventSession{ID: "codex:00000000-0000-4000-8000-000000000003",
			Generation: "00000000-0000-4000-8000-000000000004", Previous: model.StateReady, State: model.StateBusy}}
}

func TestMetadataEventValidation(t *testing.T) {
	if err := validMetadataEvent().Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		change func(*model.Event)
	}{
		{"missing event id", func(e *model.Event) { e.ID = "" }},
		{"kind", func(e *model.Event) { e.Type = "message.body" }},
		{"event id", func(e *model.Event) { e.ID = "../event" }},
		{"transaction id", func(e *model.Event) { e.TransactionID = "" }},
		{"generation", func(e *model.Event) { e.Session.Generation = "bad" }},
		{"session id", func(e *model.Event) { e.Session.ID = "display-name" }},
		{"session kind", func(e *model.Event) { e.Session.ID = "../bad:" + e.ID }},
		{"noncanonical id", func(e *model.Event) {
			e.Session.ID = "codex:" + strings.Repeat("A", 8) + "-0000-4000-8000-000000000003"
		}},
		{"previous", func(e *model.Event) { e.Session.Previous = "unknown" }},
		{"state", func(e *model.Event) { e.Session.State = "done" }},
		{"no change", func(e *model.Event) { e.Session.State = e.Session.Previous }},
		{"time", func(e *model.Event) { e.OccurredAt = "yesterday" }},
		{"zero time", func(e *model.Event) { e.OccurredAt = "0001-01-01T00:00:00Z" }},
		{"non UTC", func(e *model.Event) { e.OccurredAt = "2026-11-07T11:00:00+01:00" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := validMetadataEvent()
			test.change(&e)
			if err := e.Validate(); err == nil {
				t.Fatal("invalid event accepted")
			}
		})
	}
}

func TestMetadataRegistrationAndRetirementFacts(t *testing.T) {
	for _, change := range []func(*model.Event){
		func(e *model.Event) {
			e.Type = "session.registered"
			e.Session.Previous = ""
			e.Session.State = model.StateReady
		},
		func(e *model.Event) { e.Type = "session.registered"; e.Session.State = e.Session.Previous },
		func(e *model.Event) { e.Session.State = model.StateRetired; e.Session.Retired = true },
		func(e *model.Event) { e.Session.State = e.Session.Previous; e.Session.Retired = true },
	} {
		e := validMetadataEvent()
		change(&e)
		if err := e.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

// Every cause a hook or cleanup step writes is a valid cause of an event, and a
// name that is not one is refused: a reader that validates events (the plugin
// stream does) accepts "activity", and nothing else it does not know.
func TestEventCauseValues(t *testing.T) {
	for _, cause := range []model.EventCause{"", model.CauseActivity, model.CauseRegister, model.CauseStop, model.CauseInterrupt,
		model.CausePrompt, model.CauseTool, model.CausePermission, model.CauseSessionEnd, model.CauseOwnerExited, model.CauseCleanup, model.CauseRecovery} {
		e := validMetadataEvent()
		e.Session.Cause = cause
		if err := e.Validate(); err != nil {
			t.Errorf("cause %q refused: %v", cause, err)
		}
	}
	for _, cause := range []model.EventCause{"Activity", "model_call", "tool ", "unknown"} {
		e := validMetadataEvent()
		e.Session.Cause = cause
		if err := e.Validate(); err == nil {
			t.Errorf("cause %q accepted", cause)
		}
	}
	if model.CauseActivity != "activity" {
		t.Fatalf("CauseActivity = %q: the published fixture and plugins use the string", model.CauseActivity)
	}
}
