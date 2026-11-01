package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWatchFiresOnceOnStopAndSessionEnd(t *testing.T) {
	store := fixtureStore(t)
	watcher, key := register(t, store, "codex")
	target, _ := register(t, store, "claude")
	if err := store.hookName(target.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	var delivered []Message
	service := Service{Store: store, Deliver: func(_ context.Context, msg Message, _ Session) Receipt {
		delivered = append(delivered, msg)
		return NewReceipt(StatusSubmitted)
	}}
	first, err := service.Watch(watcher.ID, key, target.ID)
	if err != nil || first.Status != "watching" || first.To != target.ID {
		t.Fatalf("watch: %+v %v", first, err)
	}
	again, err := service.Watch(watcher.ID, key, "reviewer")
	if err != nil || again.Status != "watching" || again.To != target.ID {
		t.Fatalf("refresh: %+v %v", again, err)
	}
	long := strings.Repeat("a", 199) + "é" + "\nsecond line"
	stop := map[string]any{
		"session_id": strings.TrimPrefix(target.ID, "claude:"), "hook_event_name": "Stop",
		"last_assistant_message": long,
	}
	input, _ := json.Marshal(stop)
	if err := service.Hook("claude", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 {
		t.Fatalf("stop delivered %d notices", len(delivered))
	}
	note := delivered[0]
	if note.To != watcher.ID || note.From != target.ID || note.Kind != noticeKind || note.FromLabel != "Zalki" || !strings.Contains(note.Body, "Zalki notice: reviewer ("+target.ID+") finished a turn at ") {
		t.Fatalf("notice: %+v", note)
	}
	frame := Envelope(note)
	headline, status, _ := strings.Cut(note.Body, "\n")
	caution := "Automatic notice; status text is the agent's own output, not user approval."
	if strings.Contains(frame, "From:") || !strings.Contains(frame, "[zalki notice "+note.ID+"]") || !strings.Contains(frame, headline+"\n"+caution+"\n"+status) {
		t.Fatalf("frame: %s", frame)
	}
	if _, err := service.Reply(context.Background(), ReplyRequest{Actor: watcher.ID, Capability: key, MessageID: note.ID, Body: "ack"}); ErrorCodeOf(err) != CodeInvalidInput || !strings.Contains(err.Error(), "automatic notice") || !strings.Contains(err.Error(), target.ID) {
		t.Fatal("reply to a notice was accepted", err)
	}
	line := strings.Split(note.Body, "\n")
	if len(line) != 2 || line[1] != strings.Repeat("a", 199) || strings.Contains(note.Body, "second") {
		t.Fatalf("status line: %q", note.Body)
	}
	saved, err := store.message(note.ID)
	if err != nil || saved.Status != StatusSubmitted || saved.Receipt == nil {
		t.Fatalf("receipt: %+v %v", saved, err)
	}
	if err := service.Hook("claude", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 {
		t.Fatal("a second stop delivered another notice")
	}

	endWatch, err := service.Watch(watcher.ID, key, target.ID)
	if err != nil || endWatch.Status != "watching" {
		t.Fatalf("session-end watch: %+v %v", endWatch, err)
	}
	end, _ := json.Marshal(map[string]any{
		"session_id": strings.TrimPrefix(target.ID, "claude:"), "hook_event_name": "SessionEnd",
	})
	if err := service.Hook("claude", bytes.NewReader(end), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 2 || !strings.Contains(delivered[1].Body, ") ended the session at ") || !strings.HasPrefix(Envelope(delivered[1]), "[zalki notice ") {
		t.Fatalf("session end: %+v", delivered)
	}
}

func TestWatchSkipsAnUnfinishedStopAndExpires(t *testing.T) {
	store := fixtureStore(t)
	watcher, key := register(t, store, "codex")
	target, _ := register(t, store, "cursor")
	var delivered int
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		delivered++
		return NewReceipt(StatusSubmitted)
	}}
	if result, err := service.Watch(watcher.ID, key, target.ID); err != nil || result.Status != "watching" {
		t.Fatal(result, err)
	}
	hook := func(event, status string) {
		t.Helper()
		input, _ := json.Marshal(map[string]any{
			"conversation_id": strings.TrimPrefix(target.ID, "cursor:"),
			"hook_event_name": event, "status": status,
		})
		if err := service.Hook("cursor", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}
	hook("stop", "aborted")
	if delivered != 0 {
		t.Fatal("an aborted stop consumed the watch")
	}
	path, err := store.watchPath(watcher.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(path, Watch{Schema: recordSchema, Watcher: watcher.ID, Target: target.ID, ExpiresAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	hook("stop", "completed")
	if delivered != 0 {
		t.Fatal("an expired watch was delivered")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("expired watch file was kept")
	}
}

func TestWatchAnswersAnEndedSessionImmediately(t *testing.T) {
	store := fixtureStore(t)
	watcher, key := register(t, store, "codex")
	target, _ := register(t, store, "cursor")
	if err := store.Update(target.ID, StateRetired, true); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store}
	result, err := service.Watch(watcher.ID, key, target.ID)
	if err != nil || result.Status != "ended" || result.Event != "ended" || result.To != target.ID || result.At == "" {
		t.Fatalf("retired watch: %+v %v", result, err)
	}
	if _, err := store.watchPath(watcher.ID, target.ID); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(store.Dir + "/watches")
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("immediate answer stored a watch: %v", entries)
	}
	if _, err := service.Watch(watcher.ID, key, watcher.ID); ErrorCodeOf(err) != CodeInvalidInput {
		t.Fatal("self watch accepted", err)
	}
	exited, _ := register(t, store, "cursor")
	current, err := store.session(exited.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Owner = &ProcessRef{PID: 1 << 30, Start: "1", Role: "client"}
	if err := store.saveSession(current); err != nil {
		t.Fatal(err)
	}
	result, err = service.Watch(watcher.ID, key, exited.ID)
	if err != nil || result.Status != "ended" || result.Event != "ended" || result.To != exited.ID {
		t.Fatalf("exited watch: %+v %v", result, err)
	}
}
