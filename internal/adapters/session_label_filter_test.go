package adapters_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestCodexUnlabeledPromptFilterSemantics(t *testing.T) {
	for _, state := range []string{"ack", "stale", "current"} {
		t.Run(state, func(t *testing.T) { checkUnlabeledPromptState(t, state) })
	}
}

func checkUnlabeledPromptState(t *testing.T, state string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CODEX_HOME", root)
	s := adapterTestStore(t)
	from, key := registerAdapterSession(t, s, "cursor")
	to, receiverKey := registerAdapterSession(t, s, "codex")
	calls := 0
	service := bus.Service{Store: s, Deliver: func(context.Context, model.Message, model.Session) model.Receipt {
		calls++
		return model.NewReceipt(model.StatusSubmitted)
	}}
	message, err := service.Send(t.Context(), bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, Body: "fixture"})
	if err != nil || calls != 1 {
		t.Fatal("initial native submission missing", err, calls)
	}
	old := model.Envelope(message)
	message.FromLabel = ""
	path := filepath.Join(s.Dir, "messages", message.ID+".json")
	if err := writeTestJSON(path, message); err != nil {
		t.Fatal(err)
	}
	prepareUnlabeledPromptState(t, s, to, receiverKey, message.ID, state)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if stopped := unlabeledPromptStopped(t, &service, to.ID, old); stopped {
		t.Fatal("LEGACY_PRESENTATION_WAS_FILTERED")
	}
	canonical := model.Envelope(message)
	if stopped := unlabeledPromptStopped(t, &service, to.ID, canonical); stopped != (state != "current") {
		t.Fatal("COMPLETED_AGENT_PROMPT_NOT_STOPPED", state, stopped)
	}
	if stopped := unlabeledPromptStopped(t, &service, to.ID, canonical+"\nowner text"); stopped {
		t.Fatal("ALTERED_AGENT_PRESENTATION_WAS_FILTERED")
	}
	after, err := os.ReadFile(path)
	files, readErr := os.ReadDir(root)
	if err != nil || !bytes.Equal(before, after) || calls != 1 || readErr != nil || len(files) != 0 {
		t.Fatal("NATIVE_FILTER_RESUBMITTED_OR_CHANGED_RECEIPT", err, calls, readErr)
	}
}

func prepareUnlabeledPromptState(t *testing.T, s bus.Store, to model.Session, key, id, state string) {
	t.Helper()
	if state == "ack" {
		if _, err := s.Ack(to.ID, key, id); err != nil {
			t.Fatal(err)
		}
	}
	if state == "stale" {
		if _, _, err := s.Register(to.Kind, to.NativeID, to.CWD, nil); err != nil {
			t.Fatal(err)
		}
	}
}

func unlabeledPromptStopped(t *testing.T, service *bus.Service, actor, prompt string) bool {
	t.Helper()
	input, err := json.Marshal(agent.HookInput{SessionID: strings.TrimPrefix(actor, "codex:"), Event: "UserPromptSubmit", Prompt: prompt})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := service.Hook(t.Context(), "codex", bytes.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	if output.Len() == 0 {
		return false
	}
	if strings.TrimSpace(output.String()) != `{"continue":false}` {
		t.Fatal("unexpected native filter output", output.String())
	}
	return true
}
