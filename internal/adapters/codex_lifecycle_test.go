package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

// These tests use the actual Builtin registry through Service.Hook. Only delivery
// acceptance is stubbed; socket and queue behavior have separate integration tests.
func checkCodexPrompt(t *testing.T, service *bus.Service, actor, prompt string, stopped bool) {
	t.Helper()
	input, _ := json.Marshal(bus.HookInput{SessionID: strings.TrimPrefix(actor, "codex:"), Event: "UserPromptSubmit", Prompt: prompt})
	var out bytes.Buffer
	if err := service.Hook("codex", bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	if !stopped {
		if out.Len() != 0 {
			t.Fatalf("ordinary or unread prompt blocked: %q", out.String())
		}
		return
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["continue"] != false || len(result) != 1 {
		t.Fatalf("completed queue item was not stopped quietly: %q %v", out.String(), err)
	}
}

func TestCodexPromptFiltersOnlyCompletedExactEnvelopes(t *testing.T) {
	store := adapterTestStore(t)
	from, key := registerAdapterSession(t, store, "cursor")
	to, toKey := registerAdapterSession(t, store, "codex")
	other, _ := registerAdapterSession(t, store, "codex")
	service := bus.Service{Store: store, Deliver: func(context.Context, bus.Message, bus.Session) bus.Receipt {
		return bus.Receipt{Status: "submitted"}
	}}
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newTestID(), Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}

	checkCodexPrompt(t, &service, to.ID, bus.Envelope(msg), false)
	if _, err := store.Ack(to.ID, toKey, msg.ID); err != nil {
		t.Fatal(err)
	}
	checkCodexPrompt(t, &service, to.ID, bus.Envelope(msg), true)
	checkCodexPrompt(t, &service, to.ID, "Please explain this message:\n"+bus.Envelope(msg), false)
	checkCodexPrompt(t, &service, to.ID, bus.Envelope(msg)+"\nPlease do the work again.", false)
	checkCodexPrompt(t, &service, to.ID, strings.Replace(bus.Envelope(msg), "\nhello\n", "\nchanged\n", 1), false)
	checkCodexPrompt(t, &service, to.ID, strings.Replace(bus.Envelope(msg), `"unnamed"`, `"Different label"`, 1), false)
	checkCodexPrompt(t, &service, other.ID, bus.Envelope(msg), false)
	checkCodexPrompt(t, &service, to.ID, "[zalki peer message invalid]\nordinary text", false)
	unknown := msg
	unknown.ID = newTestID()
	checkCodexPrompt(t, &service, to.ID, bus.Envelope(unknown), false)
}

func TestCodexPromptDoesNotReplayPreviousRegistration(t *testing.T) {
	store := adapterTestStore(t)
	from, key := registerAdapterSession(t, store, "cursor")
	to, _ := registerAdapterSession(t, store, "codex")
	service := bus.Service{Store: store, Deliver: func(context.Context, bus.Message, bus.Session) bus.Receipt {
		return bus.Receipt{Status: "submitted"}
	}}
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newTestID(), Body: "previous registration"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register("codex", strings.TrimPrefix(to.ID, "codex:"), to.CWD, nil); err != nil {
		t.Fatal(err)
	}

	for range 7 {
		input, _ := json.Marshal(bus.HookInput{SessionID: strings.TrimPrefix(to.ID, "codex:"), Event: "UserPromptSubmit", Prompt: bus.Envelope(msg)})
		var out bytes.Buffer
		if err := service.Hook("codex", bytes.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out.String()) != `{"continue":false}` {
			t.Fatalf("stale native item produced a repeated blocking error: %q", out.String())
		}
	}
	stored := adapterTestMessage(t, store, msg.ID)
	if stored.AcknowledgedAt != "" || stored.Status != "submitted" {
		t.Fatalf("stopping stale input must not forge a model acknowledgement: %+v", stored)
	}
}

func codexStartHook(t *testing.T, service *bus.Service, target bus.Session, source string) (bus.Session, string) {
	t.Helper()
	input, _ := json.Marshal(bus.HookInput{SessionID: strings.TrimPrefix(target.ID, "codex:"), CWD: target.CWD, Event: "SessionStart", Source: source})
	var out bytes.Buffer
	if err := service.Hook("codex", bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Output struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`private capability is ([a-f0-9-]+)\.`).FindStringSubmatch(result.Output.Context)
	if len(match) != 2 {
		t.Fatal("startup did not provide a private capability")
	}
	current := adapterTestSession(t, service.Store, target.ID)
	return current, match[1]
}

func TestCodexCompactionPreservesQueuedMessageRegistration(t *testing.T) {
	store := adapterTestStore(t)
	from, key := registerAdapterSession(t, store, "cursor")
	to, oldKey := registerAdapterSession(t, store, "codex")
	if err := store.Update(to.ID, "busy", false); err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store, Deliver: func(context.Context, bus.Message, bus.Session) bus.Receipt {
		return bus.Receipt{Status: "submitted"}
	}}
	msg, err := service.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newTestID(), Body: "queued before compaction"})
	if err != nil {
		t.Fatal(err)
	}
	var newKey string
	previousKey := oldKey
	for range 3 {
		current, capability := codexStartHook(t, &service, to, "compact")
		if current.Generation != to.Generation || current.State != "busy" {
			t.Fatal("compaction replaced the active receiving registration")
		}
		if capability == previousKey {
			t.Fatal("compaction did not refresh the private model capability")
		}
		if _, err := store.Messages(to.ID, previousKey); err == nil {
			t.Fatal("previous compaction capability remained valid")
		}
		checkCodexPrompt(t, &service, to.ID, bus.Envelope(msg), false)
		stored := adapterTestMessage(t, store, msg.ID)
		if stored.TargetGeneration != to.Generation || stored.Status != bus.StatusSubmitted || stored.AcknowledgedAt != "" {
			t.Fatal("compaction changed the pending native delivery")
		}
		previousKey = capability
		newKey = capability
	}
	if _, err := store.Messages(to.ID, oldKey); err == nil {
		t.Fatal("superseded private capability remained valid")
	}
	if _, err := store.Ack(to.ID, newKey, msg.ID); err != nil {
		t.Fatalf("compacted session could not acknowledge its queued message: %v", err)
	}
}

func TestCodexStartupAndResumeStillReplaceRegistration(t *testing.T) {
	for _, source := range []string{"startup", "resume", ""} {
		t.Run(source, func(t *testing.T) {
			store := adapterTestStore(t)
			to, oldKey := registerAdapterSession(t, store, "codex")
			service := bus.Service{Store: store}
			current, key := codexStartHook(t, &service, to, source)
			if current.Generation == to.Generation {
				t.Fatal("new launch inherited the previous registration")
			}
			if _, err := store.Messages(to.ID, oldKey); err == nil {
				t.Fatal("previous registration's capability remained valid")
			}
			if _, err := store.Messages(to.ID, key); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCodexCompactionDoesNotReviveRetiredRegistration(t *testing.T) {
	store := adapterTestStore(t)
	to, _ := registerAdapterSession(t, store, "codex")
	if err := store.Update(to.ID, "retired", true); err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	current, _ := codexStartHook(t, &service, to, "compact")
	if current.Generation == to.Generation || current.Retired {
		t.Fatal("compaction revived a retired registration")
	}
}
