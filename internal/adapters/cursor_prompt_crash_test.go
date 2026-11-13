package adapters_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

type crashingCursorWriter struct{}

func (crashingCursorWriter) Write([]byte) (int, error) { os.Exit(23); return 0, nil }

func TestCursorRecoveryCrashAfterClaimDoesNotRetry(t *testing.T) {
	if dir := os.Getenv("ZALKI_CURSOR_CLAIM_CRASH_STATE"); dir != "" {
		store := adapterTestStore(t)
		store.Dir = dir
		service := bus.Service{Store: store}
		input := `{"conversation_id":"` + os.Getenv("ZALKI_CURSOR_CLAIM_CRASH_NATIVE") + `","hook_event_name":"postToolUse"}`
		if err := service.Hook(t.Context(), "cursor", strings.NewReader(input), crashingCursorWriter{}); err != nil {
			t.Fatal(err)
		}
		t.Fatal("C19_CLAIM_CRASH_POINT_NOT_REACHED")
	}
	fixture := newResumedCursorFixture(t)
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestCursorRecoveryCrashAfterClaimDoesNotRetry$", "-test.count=1")
	child.Env = append(os.Environ(), "ZALKI_CURSOR_CLAIM_CRASH_STATE="+fixture.service.Store.Dir, "ZALKI_CURSOR_CLAIM_CRASH_NATIVE="+fixture.actor.NativeID)
	var output bytes.Buffer
	child.Stdout = &output
	err := child.Run()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 || output.Len() != 0 {
		t.Fatal("C19_CRASH_AFTER_CLAIM_BEFORE_OUTPUT", err)
	}
	current := adapterTestSession(t, fixture.service.Store, fixture.actor.ID)
	var state map[string]bool
	if err := json.Unmarshal(current.AdapterState, &state); err != nil {
		t.Fatal(err)
	}
	if !state["recovery_attempted"] || current.Generation != fixture.actor.Generation {
		t.Fatal("C19_CRASH_PRESERVES_ONE_ATTEMPT_AND_GENERATION")
	}
	stored := adapterTestMessage(t, fixture.service.Store, fixture.message.ID)
	if stored.Status != fixture.message.Status || stored.AcknowledgedAt != "" {
		t.Fatal("C19_CRASH_PRESERVES_PENDING_MESSAGE")
	}
	text := cursorHook(t, fixture.service, fixture.actor.ID, "postToolUse", "", 0)
	if strings.Contains(text, "refreshed private capability") || !strings.Contains(text, fixture.message.Body) {
		t.Fatal("C19_CRASH_DOES_NOT_CREATE_RETRY_TURN")
	}
}
