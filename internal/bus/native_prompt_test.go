package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func TestReceiptReadDoesNotCreateSecondNativeInbox(t *testing.T) {
	for _, kind := range []string{"codex", "claude", "cursor"} {
		t.Run(kind, func(t *testing.T) {
			store := fixtureStore(t)
			from, key := register(t, store, "cursor")
			to, toKey := register(t, store, kind)
			service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
				return Receipt{Status: "submitted"}
			}}
			msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "handle once"})
			if err != nil {
				t.Fatal(err)
			}
			incoming, err := store.QueryMessages(MessageQuery{Actor: to.ID, Capability: toKey, History: false, Limit: 0})
			if err != nil || len(incoming) != 1 {
				t.Fatalf("%+v %v", incoming, err)
			}
			if kind == "cursor" {
				if incoming[0].Body != msg.Body || incoming[0].BodyDeferred {
					t.Fatal("Cursor wake hint lost its only inbox read path")
				}
			} else if incoming[0].Body != "" || !incoming[0].BodyDeferred {
				t.Fatal("receipt read exposed a native message before its queued delivery")
			}
			outgoing, err := store.Messages(from.ID, key)
			if err != nil || len(outgoing) != 1 || outgoing[0].Body != msg.Body || outgoing[0].BodyDeferred {
				t.Fatalf("sender lost its receipt/body: %+v %v", outgoing, err)
			}
			persisted, err := store.message(msg.ID)
			if err != nil || persisted.Body != msg.Body || persisted.BodyDeferred {
				t.Fatalf("read-only projection changed durable contents: %+v %v", persisted, err)
			}
			if _, err := store.Ack(to.ID, toKey, msg.ID); err != nil {
				t.Fatal(err)
			}
			history, err := store.Messages(to.ID, toKey)
			if err != nil || len(history) != 1 || history[0].Body != msg.Body || history[0].BodyDeferred {
				t.Fatalf("acknowledged history lost content: %+v %v", history, err)
			}
		})
	}
}

func TestCodexPromptFiltersOnlyCompletedExactEnvelopes(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "cursor")
	to, toKey := register(t, store, "codex")
	other, _ := register(t, store, "codex")
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		return Receipt{Status: "submitted"}
	}}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	checkHook := func(actor, prompt string, stopped bool) {
		t.Helper()
		input, _ := json.Marshal(HookInput{SessionID: strings.TrimPrefix(actor, "codex:"), Event: "UserPromptSubmit", Prompt: prompt})
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
	checkHook(to.ID, Envelope(msg), false)
	if _, err := store.Ack(to.ID, toKey, msg.ID); err != nil {
		t.Fatal(err)
	}
	checkHook(to.ID, Envelope(msg), true)
	checkHook(to.ID, "Please explain this message:\n"+Envelope(msg), false)
	checkHook(to.ID, Envelope(msg)+"\nPlease do the work again.", false)
	checkHook(to.ID, strings.Replace(Envelope(msg), "\nhello\n", "\nchanged\n", 1), false)
	checkHook(to.ID, strings.Replace(Envelope(msg), `"unnamed"`, `"Different label"`, 1), false)
	checkHook(other.ID, Envelope(msg), false)
	checkHook(to.ID, "[zalki peer message invalid]\nordinary text", false)
	unknown := msg
	unknown.ID = newID()
	checkHook(to.ID, Envelope(unknown), false)
}

func TestCodexPromptDoesNotReplayPreviousRegistration(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "cursor")
	to, _ := register(t, store, "codex")
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		return Receipt{Status: "submitted"}
	}}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "previous registration"})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register("codex", strings.TrimPrefix(to.ID, "codex:"), to.CWD, nil); err != nil {
		t.Fatal(err)
	}
	if reason, err := store.completedNativePrompt(to.ID, Envelope(msg)); err != nil || !strings.Contains(reason, "previous registration") {
		t.Fatalf("stale native message was not filtered: %q %v", reason, err)
	}
	for range 7 {
		input, _ := json.Marshal(HookInput{SessionID: strings.TrimPrefix(to.ID, "codex:"), Event: "UserPromptSubmit", Prompt: Envelope(msg)})
		var out bytes.Buffer
		if err := service.Hook("codex", bytes.NewReader(input), &out); err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(out.String()) != `{"continue":false}` {
			t.Fatalf("stale native item produced a repeated blocking error: %q", out.String())
		}
	}
	stored, err := store.message(msg.ID)
	if err != nil || stored.AcknowledgedAt != "" || stored.Status != "submitted" {
		t.Fatalf("stopping stale input must not forge a model acknowledgement: %+v %v", stored, err)
	}
}

func codexStartHook(t *testing.T, service *Service, target Session, source string) (Session, string) {
	t.Helper()
	input, _ := json.Marshal(HookInput{SessionID: strings.TrimPrefix(target.ID, "codex:"), CWD: target.CWD, Event: "SessionStart", Source: source})
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
	current, err := service.Store.session(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	return current, match[1]
}

func TestCodexCompactionPreservesQueuedMessageRegistration(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "cursor")
	to, oldKey := register(t, store, "codex")
	if err := store.Update(to.ID, "busy", false); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		return Receipt{Status: "submitted"}
	}}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "queued before compaction"})
	if err != nil {
		t.Fatal(err)
	}
	var newKey string
	for range 3 {
		current, capability := codexStartHook(t, &service, to, "compact")
		if current.Generation != to.Generation || current.State != "busy" {
			t.Fatal("compaction replaced the active receiving registration")
		}
		if capability == oldKey {
			t.Fatal("compaction did not refresh the private model capability")
		}
		if reason, err := store.completedNativePrompt(to.ID, Envelope(msg)); err != nil || reason != "" {
			t.Fatalf("valid queued input became stale after compaction: %q %v", reason, err)
		}
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
			store := fixtureStore(t)
			to, oldKey := register(t, store, "codex")
			service := Service{Store: store}
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
	store := fixtureStore(t)
	to, _ := register(t, store, "codex")
	if err := store.Update(to.ID, "retired", true); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	current, _ := codexStartHook(t, &service, to, "compact")
	if current.Generation == to.Generation || current.Retired {
		t.Fatal("compaction revived a retired registration")
	}
}
