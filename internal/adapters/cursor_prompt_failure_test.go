package adapters_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
)

func captureCursorStderr(t *testing.T, run func() error) (string, error) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = original; _ = reader.Close(); _ = writer.Close() }()
	result := run()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw), result
}

func TestCursorPromptRegistrationFailureKeepsPromptAllowed(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsafe", true: "limited"}[limited], func(t *testing.T) { assertCursorPromptFailure(t, limited) })
	}
}

func assertCursorPromptFailure(t *testing.T, limited bool) {
	t.Helper()
	for _, key := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	store := adapterTestStore(t)
	registerAdapterSession(t, store, "codex") // Create a private state directory.
	raw := []byte("{broken")
	message := "Zalki could not safely register this chat. Run zalki doctor.\n"
	if limited {
		rows := make([]map[string]string, 0, 20)
		for range 20 {
			rows = append(rows, map[string]string{"actor": "cursor:" + newTestID(), "at": time.Now().UTC().Format(time.RFC3339Nano)})
		}
		var err error
		raw, err = json.Marshal(map[string]any{"schema": 1, "attempts": rows})
		if err != nil {
			t.Fatal(err)
		}
		message = "Zalki setup is limited right now. Wait 60 seconds, then send another message.\n"
	}
	path := filepath.Join(store.Dir, "hook-registration-quota.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	native := newTestID()
	input, err := json.Marshal(map[string]string{"conversation_id": native, "cwd": "/workspace", "hook_event_name": "beforeSubmitPrompt"})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	stderr, err := captureCursorStderr(t, func() error { return service.Hook(t.Context(), "cursor", bytes.NewReader(input), &output) })
	if err != nil || strings.TrimSpace(output.String()) != `{"continue":true}` || stderr != message {
		t.Fatal("C09_REGISTRATION_FAILURE_ALLOWS_PROMPT_WITH_SAFE_DIAGNOSTIC", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) {
		t.Fatal("C09_FAILED_STATE_NOT_RESET", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "sessions", "cursor_"+native+".json")); !os.IsNotExist(err) {
		t.Fatal("C09_FAILURE_DID_NOT_REGISTER", err)
	}
}

type failingCursorWriter struct{ secret string }

func (w *failingCursorWriter) Write(raw []byte) (int, error) {
	pattern := regexp.MustCompile(`refreshed private capability is ([a-f0-9-]+);`)
	if parts := pattern.FindSubmatch(raw); len(parts) == 2 {
		w.secret = string(parts[1])
	}
	return 0, errors.New("simulated writer failure containing " + w.secret)
}

func TestCursorRecoveryWriterFailureDoesNotLeakOrRetry(t *testing.T) {
	fixture := newResumedCursorFixture(t)
	writer := &failingCursorWriter{}
	input := `{"conversation_id":"` + fixture.actor.NativeID + `","hook_event_name":"postToolUse"}`
	stderr, err := captureCursorStderr(t, func() error { return fixture.service.Hook(t.Context(), "cursor", strings.NewReader(input), writer) })
	if err == nil || writer.secret == "" || strings.Contains(err.Error(), writer.secret) || strings.Contains(stderr, writer.secret) || !strings.Contains(stderr, "Start a new chat to retry setup.") {
		t.Fatal("C19_FAILED_HANDOFF_HAS_SAFE_MANUAL_RECOVERY")
	}
	stored := adapterTestMessage(t, fixture.service.Store, fixture.message.ID)
	if stored.Status != fixture.message.Status || stored.AcknowledgedAt != "" {
		t.Fatal("C19_FAILED_HANDOFF_PRESERVES_PENDING")
	}
	for path, raw := range cursorJournalBytes(t, fixture.service.Store) {
		if strings.Contains(raw, writer.secret) {
			t.Fatal("C10_FAILED_HANDOFF_SECRET_NOT_AT_REST", path)
		}
	}
	out := cursorHook(t, fixture.service, fixture.actor.ID, "postToolUse", "", 0)
	if strings.Contains(out, "refreshed private capability") || !strings.Contains(out, fixture.message.Body) {
		t.Fatal("C19_NO_AUTOMATIC_SETUP_RETRY")
	}
}
