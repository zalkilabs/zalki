package bus

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// Pin the reviewed wire contract independently of the production limit.
const codexTestWireLimit = 32 * 1024 * 1024

type codexRepeatedByte byte

func (b codexRepeatedByte) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(b)
	}
	return len(p), nil
}

func codexBulkParts(id, event, field string, late bool) (string, string) {
	metadata := fmt.Sprintf(`"session_id":%q,"hook_event_name":%q,"cwd":"/fixture/workspace","session_title":"large"`, id, event)
	bulk := fmt.Sprintf(`%q:{"text":"`, field)
	end := `","nested":[true,null,-1.25e+2,{"escaped":"\\\"\u0061"}]}`
	if late {
		return "{" + bulk, end + "," + metadata + "}"
	}
	return "{" + metadata + "," + bulk, end + "}"
}

func codexBulkReader(id, event, field string, size int, late bool) io.Reader {
	prefix, suffix := codexBulkParts(id, event, field, late)
	return io.MultiReader(strings.NewReader(prefix), io.LimitReader(codexRepeatedByte('x'), int64(size)), strings.NewReader(suffix))
}

func TestCodexPostToolUseStreamingSelection(t *testing.T) {
	id := newID()
	for _, field := range []string{"tool_input", "tool_response"} {
		for _, late := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/late_%t", field, late), func(t *testing.T) {
				var event agent.HookInput
				size, _, err := decodeSelectedHookPayload(t.Context(), codexBulkReader(id, "PostToolUse", field, 9*1024*1024, late), &event, "PostToolUse")
				if err != nil || size <= maxHookPayload || event.Event != "PostToolUse" || event.SessionID != id || event.CWD != "/fixture/workspace" || event.SessionTitle == nil || *event.SessionTitle != "large" {
					t.Fatal("CODEX_BULK_METADATA_LOST", size, err)
				}
			})
		}
	}
	assertCodexBulkValuesDiscarded(t, id)
	assertCodexWireBoundary(t, id)
}

func assertCodexBulkValuesDiscarded(t *testing.T, id string) {
	t.Helper()
	// Both large values together fit the proposed wire budget. No bulk value is
	// retained, even though each is larger than the old entire-envelope budget.
	input := io.MultiReader(strings.NewReader(`{"tool_input":"`), io.LimitReader(codexRepeatedByte('x'), 9*1024*1024), strings.NewReader(`","tool_response":"`), io.LimitReader(codexRepeatedByte('y'), 9*1024*1024), strings.NewReader(`","hook_event_name":"PostToolUse","session_id":"`+id+`"}`))
	selector := hookPayloadSelector{input: bufio.NewReader(input)}
	selector.object()
	if selector.err != nil || len(selector.kept) > 512 || cap(selector.kept) > 1024 || bytes.Contains(selector.kept, []byte("tool_")) {
		t.Fatal("CODEX_BULK_RETAINED", selector.err, len(selector.kept), cap(selector.kept))
	}
	var selected map[string]any
	if err := json.Unmarshal(selector.kept, &selected); err != nil || selected["session_id"] != id || len(selected) != 2 {
		t.Fatal("selector removed fields other than top-level bulk", err)
	}
}

func assertCodexWireBoundary(t *testing.T, id string) {
	t.Helper()
	for _, extra := range []int{0, 1} {
		prefix, suffix := codexBulkParts(id, "PostToolUse", "tool_response", true)
		var event agent.HookInput
		size, _, err := decodeSelectedHookPayload(t.Context(), codexBulkReader(id, "PostToolUse", "tool_response", codexTestWireLimit+extra-len(prefix)-len(suffix), true), &event, "PostToolUse")
		if extra == 0 && (err != nil || size != codexTestWireLimit) || extra == 1 && model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatal("CODEX_WIRE_BOUNDARY", size, extra, err)
		}
	}
}

func TestCodexPostToolUseSelectorRefusesInvalid(t *testing.T) {
	for _, value := range []string{`"ok"`, `{"x":[true,false,null,-0.25e+12]}`, `0`, `1e999999`, `"\uD800"`, `"bad\q"`, `"\u12xz"`, `"truncated`, `{"x":1,}`, `[1,]`, `01`, `1.`, `1e+`, `true false`, `!`, ``, `{"x" 1}`, "\"bad\x01\""} {
		t.Run(fmt.Sprintf("value_%x", []byte(value)), func(t *testing.T) {
			input := `{"tool_response":` + value + `,"hook_event_name":"PostToolUse"}`
			var event agent.HookInput
			_, _, err := decodeSelectedHookPayload(t.Context(), strings.NewReader(input), &event, "PostToolUse")
			if (err == nil) != json.Valid([]byte(input)) {
				t.Fatal("CODEX_IGNORED_JSON_NOT_VALIDATED", err)
			}
		})
	}
	for _, input := range []string{
		`{"tool_response":1,"tool_response":2,"hook_event_name":"PostToolUse"}`,
		`{"hook_event_name":"Stop","hook_event_name":"PostToolUse"}`,
		`{"hook_event_name":"PostToolUse","HOOK_EVENT_NAME":"PostToolUse"}`,
		`{"session_id":"a","sess\u0069on_id":"b","hook_event_name":"PostToolUse"}`,
		`{"session_id":"a","\u017FeSSion_id":"b","hook_event_name":"PostToolUse"}`,
		`{"hook_event_name":"PostToolUse","tool_response":0,}`,
		`[]`, `null`,
	} {
		var event agent.HookInput
		if _, _, err := decodeSelectedHookPayload(t.Context(), strings.NewReader(input), &event, "PostToolUse"); err == nil {
			t.Fatal("CODEX_AMBIGUOUS_OBJECT_ACCEPTED")
		}
	}
	for _, eventName := range []string{"Stop", "SessionStart", "UserPromptSubmit", "unknown"} {
		var event agent.HookInput
		if _, _, err := decodeSelectedHookPayload(t.Context(), codexBulkReader(newID(), eventName, "tool_response", 9*1024*1024, true), &event, "PostToolUse"); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatal("CODEX_OTHER_EVENT_BUDGET_WIDENED", eventName, err)
		}
	}
	for _, field := range []string{"status", "nested"} {
		input := io.MultiReader(strings.NewReader(`{"hook_event_name":"PostToolUse","`+field+`":{"tool_response":"`), io.LimitReader(codexRepeatedByte('x'), 9*1024*1024), strings.NewReader(`"}}`))
		var event agent.HookInput
		if _, _, err := decodeSelectedHookPayload(t.Context(), input, &event, "PostToolUse"); model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatal("CODEX_RETAINED_METADATA_BUDGET_WIDENED", err)
		}
	}
	deep := `{"hook_event_name":"PostToolUse","tool_input":` + strings.Repeat("[", maxHookDepth) + "0" + strings.Repeat("]", maxHookDepth) + "}"
	var event agent.HookInput
	if _, _, err := decodeSelectedHookPayload(t.Context(), strings.NewReader(deep), &event, "PostToolUse"); err == nil {
		t.Fatal("CODEX_NESTING_UNBOUNDED")
	}
}

func TestCodexPostToolUseLargeApprovalLifecycle(t *testing.T) {
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "claude", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	msg, err := service.Send(t.Context(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "held"})
	if err != nil || !msg.Held || len(delivered) != 0 {
		t.Fatal("approval fixture not held", err)
	}
	if err := service.Hook(t.Context(), "codex", codexBulkReader(target.NativeID, "PostToolUse", "tool_response", 9*1024*1024, true), io.Discard); err != nil {
		t.Fatal("CODEX_LARGE_POST_TOOL_REJECTED", err)
	}
	assertPayloadApprovalReleased(t, store, target, msg, delivered)
	assertCodexStreamingPolicyBound(t, &service, store, target)
	assertCodexLargeHookIdentityAndStaleEvents(t, &service, store, target)
}

func assertCodexStreamingPolicyBound(t *testing.T, service *Service, store Store, target model.Session) {
	t.Helper()
	// The envelope cannot opt another adapter or native event into streaming.
	for _, candidate := range []struct{ kind, event string }{
		{"claude", "PostToolUse"}, {"cursor", "PostToolUse"}, {"codex", "Stop"},
	} {
		before := journalSnapshot(t, store)
		if err := service.Hook(t.Context(), candidate.kind, codexBulkReader(target.NativeID, candidate.event, "tool_response", 9*1024*1024, true), io.Discard); model.ErrorCodeOf(err) != model.CodeInvalidInput || !reflect.DeepEqual(before, journalSnapshot(t, store)) {
			t.Fatal("HOOK_STREAMING_POLICY_ESCAPED_ADAPTER_OR_EVENT", candidate, err)
		}
	}
}

func assertCodexLargeHookIdentityAndStaleEvents(t *testing.T, service *Service, store Store, target model.Session) {
	t.Helper()
	current, err := store.session(target.ID)
	if err != nil || current.KeyHash != target.KeyHash || current.Generation != target.Generation || current.Name != "large" {
		t.Fatal("CODEX_LARGE_HOOK_CHANGED_IDENTITY", err)
	}
	// The parser must not turn nested identity lookalikes into registration.
	before := journalSnapshot(t, store)
	input := `{"hook_event_name":"PostToolUse","nested":{"session_id":"` + newID() + `"}}`
	if err := service.Hook(t.Context(), "codex", strings.NewReader(input), io.Discard); err == nil || !reflect.DeepEqual(before, journalSnapshot(t, store)) {
		t.Fatal("CODEX_NESTED_IDENTITY_ACCEPTED", err)
	}
	// A valid large event for an absent native ID remains a stale no-op.
	if err := service.Hook(t.Context(), "codex", codexBulkReader(newID(), "PostToolUse", "tool_input", 9*1024*1024, false), io.Discard); err != nil || !reflect.DeepEqual(before, journalSnapshot(t, store)) {
		t.Fatal("CODEX_LARGE_EVENT_CREATED_MISSING_SESSION", err)
	}
}

func TestCodexPostToolUseInvalidCannotReleaseApproval(t *testing.T) {
	for _, damage := range []string{"bulk_syntax", "late_type", "duplicate", "retained_limit", "wire_limit"} {
		t.Run(damage, func(t *testing.T) {
			assertCodexInvalidBulkCannotReleaseApproval(t, damage)
		})
	}
}

func assertCodexInvalidBulkCannotReleaseApproval(t *testing.T, damage string) {
	t.Helper()
	store := fixtureStore(t)
	sender, key, target, _ := registerPair(t, store, "claude", "codex")
	if err := store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	var delivered []string
	service := approvalService(t, store, &delivered)
	msg, err := service.Send(t.Context(), SendRequest{Actor: sender.ID, Capability: key, To: target.ID, MessageID: newID(), Body: "held"})
	if err != nil || !msg.Held {
		t.Fatal(err)
	}
	before := journalSnapshot(t, store)
	var logs, output bytes.Buffer
	store.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	service.Store = store
	prefix := fmt.Sprintf(`{"hook_event_name":"PostToolUse","session_id":%q,"tool_response":"`, target.NativeID)
	suffix := `"}`
	size := 9 * 1024 * 1024
	switch damage {
	case "bulk_syntax":
		suffix = `\q"}`
	case "late_type":
		suffix = `","status":{}}`
	case "duplicate":
		suffix = `","hook_event_name":"SessionEnd"}`
	case "wire_limit":
		size = codexTestWireLimit + 1
	case "retained_limit":
		prefix = fmt.Sprintf(`{"hook_event_name":"PostToolUse","session_id":%q,"prompt":"`, target.NativeID)
	}
	input := io.MultiReader(strings.NewReader(prefix+"private-tool-sentinel"), io.LimitReader(codexRepeatedByte('x'), int64(size)), strings.NewReader(suffix))
	if err := service.Hook(t.Context(), "codex", input, &output); err == nil || len(delivered) != 0 || output.Len() != 0 || !reflect.DeepEqual(before, journalSnapshot(t, store)) || strings.Contains(logs.String(), "private-tool-sentinel") {
		t.Fatal("CODEX_INVALID_BULK_RELEASED_OR_LEAKED")
	}
}

func TestCodexPostToolUseStreamingFraming(t *testing.T) {
	id := newID()
	tail := &countedHookInput{t: t}
	var event agent.HookInput
	if _, _, err := decodeSelectedHookPayload(t.Context(), io.MultiReader(codexBulkReader(id, "PostToolUse", "tool_input", 9*1024*1024, true), tail), &event, "PostToolUse"); err != nil || tail.read != 0 {
		t.Fatal("CODEX_SELECTOR_READ_AFTER_OBJECT", err, tail.read)
	}
	assertCodexPipeReturnsWithoutEOF(t, id)
	if _, _, err := decodeSelectedHookPayload(t.Context(), failedHookInput{}, &event, "PostToolUse"); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("CODEX_SELECTOR_LOST_READ_ERROR", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := decodeSelectedHookPayload(ctx, codexBulkReader(id, "PostToolUse", "tool_response", 9*1024*1024, false), &event, "PostToolUse"); !errors.Is(err, context.Canceled) {
		t.Fatal("CODEX_EXTENDED_READ_IGNORED_CANCEL", err)
	}
	// Whitespace never completes an object; the bounded selector must stop even
	// without EOF. The outer LimitedReader cannot consume more than wire+1.
	counted := &hookSelectorInput{Reader: codexRepeatedByte(' '), ctx: t.Context()}
	if n, _, err := decodeSelectedHookPayload(t.Context(), counted, &event, "PostToolUse"); model.ErrorCodeOf(err) != model.CodeInvalidInput || n != codexTestWireLimit+1 || counted.read != codexTestWireLimit+1 {
		t.Fatal("CODEX_SELECTOR_UNBOUNDED_READ", n, counted.read, err)
	}
}

// Count completed writes without sharing unsynchronized goroutine state.
type codexPipeWriter struct {
	pipe    *io.PipeWriter
	written atomic.Int64
	done    atomic.Bool
}

func (w *codexPipeWriter) Write(p []byte) (int, error) {
	n, err := w.pipe.Write(p)
	w.written.Add(int64(n))
	return n, err
}

// A read after every byte of the complete object is delivered would block on
// the still-open pipe. Observe that attempt without synthesizing EOF or errors.
type codexOpenPipeReader struct {
	io.Reader
	delivered   int
	expected    int
	afterObject chan struct{}
}

func (r *codexOpenPipeReader) Read(p []byte) (int, error) {
	if r.delivered >= r.expected {
		r.afterObject <- struct{}{}
	}
	n, err := r.Reader.Read(p)
	r.delivered += n
	return n, err
}

func assertCodexPipeReturnsWithoutEOF(t *testing.T, id string) {
	t.Helper()
	prefix, suffix := codexBulkParts(id, "PostToolUse", "tool_response", true)
	expected := len(prefix) + 9*1024*1024 + len(suffix)
	input, output := io.Pipe()
	writer := &codexPipeWriter{pipe: output}
	reader := &codexOpenPipeReader{Reader: input, expected: expected, afterObject: make(chan struct{}, 1)}
	written, decoded := make(chan struct{}), make(chan struct{})
	var writeErr, decodeErr error
	var size int
	var event agent.HookInput
	go func() {
		_, writeErr = io.Copy(writer, codexBulkReader(id, "PostToolUse", "tool_response", 9*1024*1024, true))
		writer.done.Store(true)
		close(written)
	}()
	go func() {
		size, _, decodeErr = decodeSelectedHookPayload(t.Context(), reader, &event, "PostToolUse")
		close(decoded)
	}()
	defer func() {
		if t.Failed() {
			t.Logf("before pipe cleanup: bytes_written=%d producer_done=%t", writer.written.Load(), writer.done.Load())
			select {
			case <-decoded:
				t.Logf("before pipe cleanup: decoder_done=true decoder_err=%v", decodeErr)
			default:
				t.Log("before pipe cleanup: decoder_done=false decoder_err=pending")
			}
		}
		_ = input.Close()
		_ = output.Close()
		<-written
		<-decoded
		if t.Failed() {
			t.Logf("after pipe cleanup: bytes_written=%d producer_done=%t write_err=%v decoder_err=%v bytes_read=%d", writer.written.Load(), writer.done.Load(), writeErr, decodeErr, reader.delivered)
		}
	}()
	awaitCodexPipeEvents(t, reader, written, decoded, &writeErr, &decodeErr)
	// Both completion events have arrived. Neither pipe end is closed until
	// deferred cleanup, so this result cannot have depended on EOF.
	if writer.written.Load() != int64(expected) || size != expected || event.SessionID != id || event.Event != "PostToolUse" {
		t.Fatal("CODEX_PIPE_OBJECT_NOT_COMPLETE", writer.written.Load(), size, expected, event.SessionID, event.Event)
	}
}

func awaitCodexPipeEvents(t *testing.T, reader *codexOpenPipeReader, written, decoded <-chan struct{}, writeErr, decodeErr *error) {
	t.Helper()
	for written != nil || decoded != nil {
		select {
		case <-written:
			written = nil
			if *writeErr != nil {
				t.Fatal("CODEX_PIPE_PRODUCER_FAILED", *writeErr)
			}
		case <-decoded:
			decoded = nil
			if *decodeErr != nil {
				t.Fatal("CODEX_PIPE_DECODER_FAILED", *decodeErr)
			}
		case <-reader.afterObject:
			t.Fatal("CODEX_SELECTOR_WAITED_FOR_EOF")
		}
	}
}
