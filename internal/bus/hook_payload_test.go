package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func hookPayload(id, event string, responseSize int) string {
	return fmt.Sprintf(`{"hook_event_name":%q,"session_id":%q,"cwd":"/fixture/workspace","tool_response":%q}`, event, id, strings.Repeat("x", responseSize))
}

func TestHookLargePayloadReleasesApproval(t *testing.T) {
	for _, size := range []int{500, 131000} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			store := fixtureStore(t)
			sender, key, target, _ := registerPair(t, store, "codex", "claude")
			var delivered []string
			service := approvalService(t, store, &delivered)
			if err := service.Hook(t.Context(), "claude", strings.NewReader(hookPayload(target.NativeID, "PermissionRequest", 0)), io.Discard); err != nil {
				t.Fatal(err)
			}
			msg, err := service.Send(t.Context(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "held"})
			if err != nil || !msg.Held {
				t.Fatalf("not held: %+v %v", msg, err)
			}
			input := hookPayload(target.NativeID, "PostToolUse", size)
			if size == 131000 && len(input) <= 128*1024 {
				t.Fatal("fixture no longer crosses old limit")
			}
			if err = service.Hook(t.Context(), "claude", strings.NewReader(input), io.Discard); err != nil {
				t.Fatal(err)
			}
			assertPayloadApprovalReleased(t, store, target, msg, delivered)
		})
	}
}

func assertPayloadApprovalReleased(t *testing.T, store Store, target model.Session, msg model.Message, delivered []string) {
	t.Helper()
	current, err := store.session(target.ID)
	if err != nil || current.State != model.StateBusy || current.ApprovalHeld != 0 || strings.Join(delivered, ",") != "held" {
		t.Fatalf("lifecycle/queue: state=%s held=%d delivered=%v err=%v", current.State, current.ApprovalHeld, delivered, err)
	}
	saved, err := store.message(msg.ID)
	if err != nil || saved.Held || saved.Status != model.StatusSubmitted {
		t.Fatalf("receipt: %+v %v", saved, err)
	}
}

func TestHookPayloadWireBoundary(t *testing.T) {
	for _, size := range []int{8 * 1024 * 1024, 8*1024*1024 + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			store := fixtureStore(t)
			target, _ := register(t, store, "claude")
			before := sessionBytes(t, store, target.ID)
			base := hookPayload(target.NativeID, "PostToolUse", 0)
			input := hookPayload(target.NativeID, "PostToolUse", size-len(base))
			if len(input) != size {
				t.Fatal("wrong wire size")
			}
			// Deliver the last in-limit byte and the over-limit byte separately.
			reader := io.MultiReader(strings.NewReader(input[:8*1024*1024-1]), strings.NewReader(input[8*1024*1024-1:8*1024*1024]), strings.NewReader(input[8*1024*1024:]))
			err := (&Service{Store: store}).Hook(t.Context(), "claude", reader, io.Discard)
			if size == 8*1024*1024 {
				current, stateErr := store.session(target.ID)
				if err != nil || stateErr != nil || current.State != model.StateBusy {
					t.Fatalf("exact cap: %v %v %s", err, stateErr, current.State)
				}
			} else if model.ErrorCodeOf(err) != model.CodeInvalidInput || err.Error() != "invalid_input: hook payload exceeds 8 MiB" || !bytes.Equal(before, sessionBytes(t, store, target.ID)) {
				t.Fatalf("over cap: %v", err)
			}
		})
	}
}

func TestHookPayloadPreservesFirstValueAndNativeErrors(t *testing.T) {
	for _, tail := range []string{"", " \n\t", " garbage", ` {"hook_event_name":"SessionEnd"}`} {
		store := fixtureStore(t)
		target, _ := register(t, store, "claude")
		err := (&Service{Store: store}).Hook(t.Context(), "claude", strings.NewReader(hookPayload(target.NativeID, "PostToolUse", 0)+tail), io.Discard)
		current, stateErr := store.session(target.ID)
		if err != nil || stateErr != nil || current.State != model.StateBusy {
			t.Fatalf("first value with %q: %v %v %s", tail, err, stateErr, current.State)
		}
	}
	for _, input := range []string{"", "{", `{"hook_event_name":"PostToolUse","status":{}}`, `[1]`, `{"bad":!}`} {
		var expected agent.HookInput
		nativeErr := json.NewDecoder(strings.NewReader(input)).Decode(&expected)
		store := fixtureStore(t)
		actualErr := (&Service{Store: store}).Hook(t.Context(), "claude", strings.NewReader(input), io.Discard)
		if fmt.Sprintf("%T:%v", actualErr, actualErr) != fmt.Sprintf("%T:%v", nativeErr, nativeErr) {
			t.Fatalf("native error changed: %v / %v", nativeErr, actualErr)
		}
	}
}

type countedHookInput struct {
	t    *testing.T
	read int
}

func (r *countedHookInput) Read(data []byte) (int, error) {
	if r.read+len(data) > 8*1024*1024+1 {
		r.t.Fatal("reader consumed beyond limit+1")
	}
	for i := range data {
		data[i] = ' '
	}
	r.read += len(data)
	return len(data), nil
}

func TestHookPayloadEndlessInputIsBounded(t *testing.T) {
	input := &countedHookInput{t: t}
	var event agent.HookInput
	size, header, err := decodeHookPayload(input, &event)
	if input.read != 8*1024*1024+1 || size != input.read || len(header) != 16*1024 || model.ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatalf("unbounded/incorrect read: %d %d %d %v", input.read, size, len(header), err)
	}
}

type failedHookInput struct{}

func (failedHookInput) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestHookPayloadReadFailureIsPreserved(t *testing.T) {
	store := fixtureStore(t)
	err := (&Service{Store: store}).Hook(t.Context(), "claude", failedHookInput{}, io.Discard)
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("input IO error changed", err)
	}
}

func TestHookPayloadFailureDiagnosticsAreBoundedAndPrivate(t *testing.T) {
	const private = "private-payload-sentinel"
	for _, test := range []struct{ name, input, event, sizeClass string }{
		{"truncated", `{"hook_event_name":"PostToolUse","tool_response":"` + private, "PostToolUse", "le_128kib"},
		{"large_truncated", `{"hook_event_name":"PostToolUse","tool_response":"` + strings.Repeat(private, 8000), "PostToolUse", "128kib_to_8mib"},
		{"oversize", `{"hook_event_name":"PostToolUse","tool_response":"` + strings.Repeat(private, 350000), "PostToolUse", "gt_8mib"},
		{"late_type", `{"tool_response":"` + strings.Repeat("x", 17000) + `","hook_event_name":"PostToolUse","status":{}}`, "PostToolUse", "le_128kib"},
		{"trailing_header_after_type_error", `{"hook_event_name":{}} {"hook_event_name":"Stop"}`, "unknown", "le_128kib"},
		{"private_event", `{"hook_event_name":"` + private + `","tool_response":"`, "unknown", "le_128kib"},
		{"nested_event", `{"nested":{"hook_event_name":"SessionEnd"},"bad":`, "unknown", "le_128kib"},
		{"late_event", `{"tool_response":"` + strings.Repeat("x", 17000) + `","hook_event_name":"Stop","bad":`, "unknown", "le_128kib"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := fixtureStore(t)
			target, _ := register(t, store, "claude")
			before := sessionBytes(t, store, target.ID)
			var diagnostic, output bytes.Buffer
			store.Logger = slog.New(slog.NewJSONHandler(&diagnostic, nil))
			err := (&Service{Store: store}).Hook(t.Context(), "claude", strings.NewReader(test.input), &output)
			if err == nil {
				t.Fatal("bad input accepted")
			}
			rows := logRows(t, &diagnostic)
			if len(rows) != 1 || rows[0]["msg"] != "hook.failed" || rows[0]["hook_stage"] != "decode" || rows[0]["hook_event"] != test.event || rows[0]["hook_payload_class"] != test.sizeClass {
				t.Fatalf("diagnostic: %v", rows)
			}
			if strings.Contains(diagnostic.String(), private) || strings.Contains(diagnostic.String(), "tool_response") || strings.Contains(diagnostic.String(), target.NativeID) || output.Len() != 0 || !bytes.Equal(before, sessionBytes(t, store, target.ID)) {
				t.Fatal("failed input leaked or mutated state")
			}
		})
	}
}

func TestHookPayloadHeaderProbe(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{`{"hook_event_name":"PostToolUse","broken":`, "PostToolUse"},
		{`{"other":[1,{"nested":true}],"hook_event_name":"Stop"}`, "Stop"},
		{`{"hook_event_name":123}`, ""},
		{`{"hook_event_name":`, ""},
		{`{"x":!}`, ""},
		{`{"broken`, ""},
		{`{"x":`, ""},
		{`{}`, ""},
		{`["hook_event_name","Stop"]`, ""},
		{`null`, ""},
		{``, ""},
		{`{"x":"hook_event_name: Stop","hook_event_name":"SessionEnd"}`, "SessionEnd"},
	} {
		if got := probeHookEvent([]byte(test.input)); got != test.want {
			t.Fatalf("probe %q: %q want %q", test.input, got, test.want)
		}
	}
}

func TestHookPayloadSizeClasses(t *testing.T) {
	for _, test := range []struct {
		size int
		want string
	}{
		{0, "le_128kib"}, {128 * 1024, "le_128kib"},
		{128*1024 + 1, "128kib_to_8mib"}, {8 * 1024 * 1024, "128kib_to_8mib"},
		{8*1024*1024 + 1, "gt_8mib"},
	} {
		if got := hookPayloadSizeClass(test.size); got != test.want {
			t.Fatalf("size %d: %s want %s", test.size, got, test.want)
		}
	}
}

func TestHookLargeStopNotifiesWatch(t *testing.T) {
	store := fixtureStore(t)
	watcher, key, target, _ := registerPair(t, store, "codex", "claude")
	var delivered []model.Message
	service := Service{Store: store, Deliver: func(_ context.Context, msg model.Message, _ model.Session) model.Receipt {
		delivered = append(delivered, msg)
		return model.NewReceipt(model.StatusSubmitted)
	}}
	if result, err := service.Watch(watcher.ID, key, target.ID); err != nil || result.Status != "watching" {
		t.Fatal("watch not armed", err)
	}
	input := hookPayload(target.NativeID, "Stop", 131000)
	if err := service.Hook(t.Context(), "claude", strings.NewReader(input), io.Discard); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 {
		t.Fatalf("watch notices: %d", len(delivered))
	}
	if delivered[0].From != target.ID || delivered[0].To != watcher.ID || delivered[0].Kind != model.NoticeKind {
		t.Fatal("wrong notice recipient/type")
	}
}

func TestHookLargeStartupKeepsRedactedNativeCapture(t *testing.T) {
	root := filepath.Join(t.TempDir(), "contracts")
	t.Setenv("ZALKI_CAPTURE_CONTRACTS", root)
	t.Setenv("PATH", t.TempDir())
	store := fixtureStore(t)
	id := newID()
	input := hookPayload(id, "SessionStart", 131000)
	var output bytes.Buffer
	if err := (&Service{Store: store}).Hook(t.Context(), "codex", strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "codex-unknown.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(id)) || bytes.Contains(raw, []byte("tool_response")) || len(raw) > 2048 {
		t.Fatal("native capture retained ignored payload or private ID")
	}
	var fixture struct {
		Hook agent.HookInput `json:"hook"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil || fixture.Hook.Event != "SessionStart" || fixture.Hook.CWD != "/fixture/workspace" || fixture.Hook.SessionID != "00000000-0000-4000-8000-000000000001" {
		t.Fatalf("capture contract: %s %v", raw, err)
	}
	current, err := store.session("codex:" + id)
	if err != nil || current.NativeID != id || output.Len() == 0 {
		t.Fatal("startup identity/output lost", err)
	}
}

func TestHookPayloadIgnoresInputAfterFirstValue(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "claude")
	tail := &countedHookInput{t: t}
	input := io.MultiReader(strings.NewReader(hookPayload(target.NativeID, "PostToolUse", 0)), tail)
	if err := (&Service{Store: store}).Hook(t.Context(), "claude", input, io.Discard); err != nil {
		t.Fatal(err)
	}
	if tail.read != 0 {
		t.Fatal("decoder read past a complete first value", tail.read)
	}
}

func TestHookPayloadTruncatedAtExactLimitKeepsEOF(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "claude")
	before := sessionBytes(t, store, target.ID)
	base := hookPayload(target.NativeID, "PostToolUse", 0)
	input := hookPayload(target.NativeID, "PostToolUse", 8*1024*1024+1-len(base))[:8*1024*1024]
	err := (&Service{Store: store}).Hook(t.Context(), "claude", strings.NewReader(input), io.Discard)
	if !errors.Is(err, io.ErrUnexpectedEOF) || !bytes.Equal(before, sessionBytes(t, store, target.ID)) {
		t.Fatal("exact-limit truncated JSON lost native EOF or mutated state", err)
	}
}

func TestHookPayloadExactLimitIgnoresTrailingReadAhead(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "claude")
	base := hookPayload(target.NativeID, "PostToolUse", 0)
	input := hookPayload(target.NativeID, "PostToolUse", 8*1024*1024-len(base)) + " trailing garbage"
	if err := (&Service{Store: store}).Hook(t.Context(), "claude", strings.NewReader(input), io.Discard); err != nil {
		t.Fatal("trailing read ahead counted against first value", err)
	}
	current, err := store.session(target.ID)
	if err != nil || current.State != model.StateBusy {
		t.Fatal("exact-limit event not applied", err)
	}
}

func TestHookPayloadTypeErrorKeepsNativeErrorWithReadAhead(t *testing.T) {
	store := fixtureStore(t)
	target, _ := register(t, store, "claude")
	before := sessionBytes(t, store, target.ID)
	suffix := `,"status":{}}`
	base := strings.TrimSuffix(hookPayload(target.NativeID, "PostToolUse", 0), "}") + suffix
	input := strings.TrimSuffix(hookPayload(target.NativeID, "PostToolUse", 8*1024*1024-len(base)), "}") + suffix + " trailing garbage"
	err := (&Service{Store: store}).Hook(t.Context(), "claude", strings.NewReader(input), io.Discard)
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) || !bytes.Equal(before, sessionBytes(t, store, target.ID)) {
		t.Fatal("read-ahead replaced native type error or mutated state", err)
	}
}

type hookPrefixInput struct {
	t       *testing.T
	read    bool
	payload string
}

func (r *hookPrefixInput) Read(data []byte) (int, error) {
	if r.read {
		r.t.Fatal("diagnostic probe read stdin after the decoder failed")
	}
	r.read = true
	return copy(data, r.payload), nil
}

func TestHookPayloadHeaderProbeDoesNotReadStdin(t *testing.T) {
	input := &hookPrefixInput{t: t, payload: `{"hook_event_name":"PostToolUse","bad":!}`}
	var event agent.HookInput
	size, header, err := decodeHookPayload(input, &event)
	var syntaxErr *json.SyntaxError
	if !errors.As(err, &syntaxErr) || size != len(input.payload) || probeHookEvent(header) != "PostToolUse" {
		t.Fatal("failed decode lost its bounded buffered header", err)
	}
}
