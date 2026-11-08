package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// These tests pin the event-data attributes constructed by Store.logEvent,
// their validation and their order. These attributes are metadata: never a body,
// capability, name, path or error text. Odd
// results are characterized, not endorsed.
//
// logEvent constructs these event-data attributes, in this order:
//  1. message_id, reply_to, generation: kept only if the value is a UUID,
//     written in lower case.
//  2. actor, recipient: kept only as "<kind>:<uuid>" with a registered adapter
//     kind and a valid UUID, written as kind:lower-case-uuid.
//  3. agent: kept only if it is a registered adapter kind.
//  4. hook_event: kept only if it is one of the standard hook events.
//  5. hook_stage: kept only if it is one of the 15 fixed hook stages.
//  6. status, receipt_status, state: kept only if they are valid values.
//  7. error_kind: the class of Err (never its text); only if Err is set.
//  8. error_code: Code, or the code of Err when Code is empty; a code outside
//     the ten known ones (including rate_limited) is written as "internal"
//     (it does not fall back to Err).
//  9. duration_ms: only if the duration is above zero.
// 10. reused=true: only if Reused is set.
// A nil logger, or a level the logger does not log, builds and emits nothing.

const (
	goodUUID   = "11111111-1111-4111-8111-111111111111"
	otherUUID  = "22222222-2222-4222-8222-222222222222"
	thirdUUID  = "33333333-3333-4333-8333-333333333333"
	shoutyUUID = "AAAAAAAA-BBBB-4CCC-8DDD-EEEEEEEEEEEE"
)

// capturedLog is one log record as the handler saw it: attributes in order.
type capturedLog struct {
	level slog.Level
	event string
	attrs []string // "key=value"
}

// captureHandler records every record in order, without formatting them.
type logCapture struct {
	asked   int
	records []capturedLog
}

type captureHandler struct {
	*logCapture
	minimum slog.Level
	bound   []string
	group   string
}

func newCaptureHandler(minimum slog.Level) *captureHandler {
	return &captureHandler{logCapture: &logCapture{}, minimum: minimum}
}

func (h *captureHandler) Enabled(_ context.Context, level slog.Level) bool {
	h.asked++
	return level >= h.minimum
}

func (h *captureHandler) Handle(_ context.Context, record slog.Record) error {
	captured := capturedLog{level: record.Level, event: record.Message, attrs: slices.Clone(h.bound)}
	record.Attrs(func(attr slog.Attr) bool {
		captured.attrs = captureLogAttrs(captured.attrs, h.group, attr)
		return true
	})
	h.records = append(h.records, captured)
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.bound = captureLogAttrs(slices.Clone(h.bound), h.group, attrs...)
	return &next
}

func (h *captureHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.group += name + "."
	return &next
}

// Capture contextual and grouped attributes too, so a logging refactor cannot
// hide a leaked value by moving it from LogAttrs into Logger.With/WithGroup.
func captureLogAttrs(dst []string, prefix string, attrs ...slog.Attr) []string {
	for _, attr := range attrs {
		attr.Value = attr.Value.Resolve()
		if attr.Equal(slog.Attr{}) {
			continue
		}
		key := prefix + attr.Key
		if attr.Value.Kind() == slog.KindGroup {
			if attr.Key != "" {
				key += "."
			}
			dst = captureLogAttrs(dst, key, attr.Value.Group()...)
			continue
		}
		dst = append(dst, key+"="+attr.Value.String())
	}
	return dst
}

// loggedAttrs runs logEvent once at Info level and returns the attributes.
func loggedAttrs(t *testing.T, data eventData) []string {
	t.Helper()
	store := fixtureStore(t)
	handler := newCaptureHandler(slog.LevelDebug)
	store.Logger = slog.New(handler)
	store.logEvent(slog.LevelInfo, "test.event", data)
	if len(handler.records) != 1 || handler.records[0].event != "test.event" || handler.records[0].level != slog.LevelInfo {
		t.Fatalf("expected one test.event record at info level, got %+v", handler.records)
	}
	return handler.records[0].attrs
}

// logCase is one input and the exact attributes it must produce.
type logCase struct {
	name string
	data eventData
	want []string
}

func assertLogCases(t *testing.T, cases []logCase) {
	t.Helper()
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := loggedAttrs(t, test.data)
			if !slices.Equal(got, test.want) {
				t.Fatalf("attributes = %v, want %v", got, test.want)
			}
			// The handler saw the real keys and values. Whatever the case, no
			// attribute may carry a secret-looking value.
			for _, attr := range got {
				if strings.Contains(strings.ToLower(attr), "secret") {
					t.Fatalf("a secret-looking value reached the handler: %s", attr)
				}
			}
		})
	}
}

func TestLogEventKeepsOnlyValidIdentifiers(t *testing.T) {
	assertLogCases(t, []logCase{
		{"ids are kept and lower-cased", eventData{MessageID: shoutyUUID, ReplyTo: otherUUID, Generation: thirdUUID},
			[]string{"message_id=aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "reply_to=" + otherUUID, "generation=" + thirdUUID}},
		{"a message id that is not a UUID is dropped", eventData{MessageID: "secret-text"}, nil},
		{"a reply_to that is not a UUID is dropped", eventData{ReplyTo: "secret-text"}, nil},
		{"a generation that is not a UUID is dropped", eventData{Generation: "secret-text"}, nil},
		{"a UUID with trailing text is dropped", eventData{MessageID: goodUUID + " secret"}, nil},
		{"actor and recipient are kept as kind:uuid", eventData{Actor: "codex:" + shoutyUUID, Recipient: "cursor:" + goodUUID},
			[]string{"actor=codex:aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", "recipient=cursor:" + goodUUID}},
		{"an actor of an unknown kind is dropped", eventData{Actor: "secret-kind:" + goodUUID}, nil},
		{"an actor without a colon is dropped", eventData{Actor: "secret"}, nil},
		{"an actor with a bad UUID is dropped", eventData{Actor: "codex:secret"}, nil},
		{"a recipient of an unknown kind is dropped", eventData{Recipient: "secret-kind:" + goodUUID}, nil},
		{"a recipient with a bad UUID is dropped", eventData{Recipient: "cursor:secret-id"}, nil},
	})
}

func TestLogEventKeepsOnlyKnownAgentsEventsAndStages(t *testing.T) {
	// Fifteen stages: "watch" and "approval" are set by Service.Hook and used to be
	// dropped from the logs (fixed together with the rate_limited code).
	stages := []string{"adapter", "decode", "capture", "event", "normalize", "identity", "owner", "handler", "watch", "approval", "register", "name", "startup", "output", "state"}
	cases := make([]logCase, 0, 8+len(stages))
	cases = append(cases, []logCase{
		{"a registered agent is kept", eventData{Agent: "cursor"}, []string{"agent=cursor"}},
		{"an unregistered agent is dropped", eventData{Agent: "secret-agent"}, nil},
		{"a standard hook event is kept", eventData{HookEvent: "Stop"}, []string{"hook_event=Stop"}},
		{"the interrupt event is kept", eventData{HookEvent: "Interrupt"}, []string{"hook_event=Interrupt"}},
		{"a hook event in another case is dropped", eventData{HookEvent: "stop"}, nil},
		{"an unknown hook event is dropped", eventData{HookEvent: "secret-event"}, nil},
		{"a stage that is not one of the fixed words is dropped", eventData{HookStage: "secret-stage"}, nil},
		{"a fixed stage in another case is dropped", eventData{HookStage: "Decode"}, nil},
	}...)
	for _, stage := range stages {
		cases = append(cases, logCase{"the stage " + stage + " is kept", eventData{HookStage: stage}, []string{"hook_stage=" + stage}})
	}
	assertLogCases(t, cases)
}

func TestLogEventKeepsOnlyValidStatusesAndStates(t *testing.T) {
	assertLogCases(t, []logCase{
		{"status, receipt status and state are kept", eventData{Status: model.StatusSubmitted, ReceiptStatus: model.StatusNotSent, State: model.StateApproval},
			[]string{"status=submitted", "receipt_status=not_sent", "state=approval"}},
		{"an invalid status is dropped", eventData{Status: "secret-status"}, nil},
		{"an invalid receipt status is dropped", eventData{ReceiptStatus: "secret-status"}, nil},
		{"an invalid state is dropped", eventData{State: "secret-state"}, nil},
		{"the empty values are dropped", eventData{}, nil},
	})
}

func TestLogEventNeverWritesErrorText(t *testing.T) {
	secret := fmt.Errorf("secret path /fixture/private/key: %w", syscall.ECONNREFUSED)
	assertLogCases(t, []logCase{
		{"an error gives its class and code, not its text", eventData{Err: secret}, []string{"error_kind=connection_refused", "error_code=internal"}},
		{"an error that is not classified is other", eventData{Err: errors.New("secret text")}, []string{"error_kind=other", "error_code=internal"}},
		{"a missing file is classified and coded", eventData{Err: fmt.Errorf("secret: %w", os.ErrNotExist)}, []string{"error_kind=not_found", "error_code=not_found"}},
		{"a permission error is coded unauthorized", eventData{Err: fmt.Errorf("secret: %w", os.ErrPermission)}, []string{"error_kind=permission_denied", "error_code=unauthorized"}},
		{"a coded error gives its code", eventData{Err: model.NewError(model.CodeConflict, "secret message")}, []string{"error_kind=other", "error_code=conflict"}},
		{"no error gives no error attributes", eventData{}, nil},
	})
}

func TestLogEventErrorCodePrecedenceAndAllowlist(t *testing.T) {
	known := []model.ErrorCode{model.CodeInvalidInput, model.CodeInvalidIdentity, model.CodeUnauthorized, model.CodeConflict, model.CodeUnavailable, model.CodeRateLimited, model.CodeNotFound, model.CodeUnsupportedSchema, model.CodeInvalidTransition, model.CodeInternal}
	cases := make([]logCase, 0, len(known)+5)
	for _, code := range known {
		cases = append(cases, logCase{"the code " + string(code) + " is kept", eventData{Code: code}, []string{"error_code=" + string(code)}})
	}
	cases = append(cases,
		logCase{"an unknown code is written as internal", eventData{Code: "secret-code"}, []string{"error_code=internal"}},
		// A rate limit used to be written as internal (the code was missing from the list).
		logCase{"a rate limit is kept", eventData{Code: model.CodeRateLimited}, []string{"error_code=rate_limited"}},
		// Two faults at once: Code is looked at first and does not fall back to Err.
		logCase{"a code set together with an error: the code wins", eventData{Code: model.CodeUnavailable, Err: model.NewError(model.CodeConflict, "x")}, []string{"error_kind=other", "error_code=unavailable"}},
		logCase{"an unknown code with a known error: internal, not the error's code", eventData{Code: "secret-code", Err: model.NewError(model.CodeConflict, "x")}, []string{"error_kind=other", "error_code=internal"}},
		logCase{"no code: the error's code", eventData{Err: model.NewError(model.CodeNotFound, "x")}, []string{"error_kind=other", "error_code=not_found"}},
	)
	assertLogCases(t, cases)
}

func TestLogEventKeepsDurationsAboveZeroOnly(t *testing.T) {
	assertLogCases(t, []logCase{
		{"two seconds", eventData{Duration: 2 * time.Second}, []string{"duration_ms=2000"}},
		{"a millisecond", eventData{Duration: time.Millisecond}, []string{"duration_ms=1"}},
		{"less than a millisecond is written as zero", eventData{Duration: time.Microsecond}, []string{"duration_ms=0"}},
		{"a single nanosecond is above zero", eventData{Duration: time.Nanosecond}, []string{"duration_ms=0"}},
		{"zero is dropped", eventData{Duration: 0}, nil},
		{"a negative duration is dropped", eventData{Duration: -time.Second}, nil},
		{"reused is kept when set", eventData{Reused: true}, []string{"reused=true"}},
		{"reused is dropped when not set", eventData{Reused: false}, nil},
	})
}

func TestLogEventWritesEverythingInOneFixedOrder(t *testing.T) {
	all := eventData{
		Actor: "codex:" + goodUUID, Recipient: "claude:" + otherUUID, MessageID: goodUUID, ReplyTo: otherUUID, Generation: thirdUUID,
		Agent: "cursor", HookEvent: "Stop", HookStage: "decode",
		Status: model.StatusSubmitted, ReceiptStatus: model.StatusNotSent, State: model.StateReady,
		Code: model.CodeConflict, Err: errors.New("secret"), Duration: 3 * time.Millisecond, Reused: true,
	}
	want := []string{
		"message_id=" + goodUUID, "reply_to=" + otherUUID, "generation=" + thirdUUID,
		"actor=codex:" + goodUUID, "recipient=claude:" + otherUUID,
		"agent=cursor", "hook_event=Stop", "hook_stage=decode",
		"status=submitted", "receipt_status=not_sent", "state=ready",
		"error_kind=other", "error_code=conflict", "duration_ms=3", "reused=true",
	}
	if got := loggedAttrs(t, all); !slices.Equal(got, want) {
		t.Fatalf("attribute order:\n got %v\nwant %v", got, want)
	}
}

func TestLogEventWithSecretsInEveryFieldEmitsOnlyTheErrorClass(t *testing.T) {
	secret := "secret-value"
	got := loggedAttrs(t, eventData{
		Actor: secret, Recipient: secret, MessageID: secret, ReplyTo: secret, Generation: secret,
		Agent: secret, HookEvent: secret, HookStage: secret,
		Status: model.DeliveryStatus(secret), ReceiptStatus: model.DeliveryStatus(secret), State: model.SessionState(secret),
		Code: model.ErrorCode(secret), Err: errors.New(secret),
	})
	want := []string{"error_kind=other", "error_code=internal"}
	if !slices.Equal(got, want) {
		t.Fatalf("attributes = %v, want only %v", got, want)
	}
	for _, attr := range got {
		if strings.Contains(attr, secret) {
			t.Fatalf("a secret reached the log: %s", attr)
		}
	}
}

func TestLogEventDoesNothingWhenTheLevelIsOffOrThereIsNoLogger(t *testing.T) {
	store := fixtureStore(t)
	store.logEvent(slog.LevelError, "test.event", eventData{MessageID: goodUUID}) // no logger: must not panic
	handler := newCaptureHandler(slog.LevelWarn)
	store.Logger = slog.New(handler)
	store.logEvent(slog.LevelInfo, "below.level", eventData{MessageID: goodUUID, Err: errors.New("x")})
	if len(handler.records) != 0 || handler.asked != 1 {
		t.Fatalf("a record below the level was handled (%d records, handler asked %d times)", len(handler.records), handler.asked)
	}
	// The gate comes before any work: a disabled record allocates nothing.
	data := eventData{MessageID: goodUUID, Actor: "codex:" + goodUUID, Err: errors.New("x"), Duration: time.Second}
	if allocs := testing.AllocsPerRun(20, func() { store.logEvent(slog.LevelInfo, "below.level", data) }); allocs != 0 {
		t.Fatalf("a record below the level allocated %v times, want 0", allocs)
	}
	store.logEvent(slog.LevelWarn, "at.level", eventData{MessageID: goodUUID})
	if len(handler.records) != 1 || handler.records[0].event != "at.level" {
		t.Fatalf("a record at the level was lost: %+v", handler.records)
	}
}

// The reused attribute reaches the log through a real double registration: a
// registration that keeps the existing one (compaction: the generation is
// preserved) is marked reused; a plain re-registration is not.
func TestOnlyAPreservingRegistrationIsLoggedAsReused(t *testing.T) {
	store := fixtureStore(t)
	handler := newCaptureHandler(slog.LevelDebug)
	store.Logger = slog.New(handler)
	id := newID()
	if _, _, err := store.Register("cursor", id, "/workspace", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.register("cursor", id, "/workspace", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.register("cursor", id, "/workspace", nil, false); err != nil {
		t.Fatal(err)
	}
	var registered []capturedLog
	for _, record := range handler.records {
		if record.event == "session.registered" {
			registered = append(registered, record)
		}
	}
	if len(registered) != 3 {
		t.Fatalf("session.registered records = %d, want 3", len(registered))
	}
	for i, wantReused := range []bool{false, true, false} {
		attrs := registered[i].attrs
		if got := slices.Contains(attrs, "reused=true"); got != wantReused {
			t.Fatalf("registration %d reused=%v, want %v: %v", i+1, got, wantReused, attrs)
		}
		if wantReused && attrs[len(attrs)-1] != "reused=true" {
			t.Fatalf("reused is not the last attribute: %v", attrs)
		}
	}
}
