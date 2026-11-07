package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestWatchFiresOnceOnStopAndSessionEnd(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	watcher, key, target, _ := registerPair(t, store, "codex", "claude")
	if err := store.hookName(target.ID, "reviewer"); err != nil {
		t.Fatal(err)
	}
	var delivered []Message
	service.Deliver = func(_ context.Context, msg Message, _ Session) Receipt {
		delivered = append(delivered, msg)
		return NewReceipt(model.StatusSubmitted)
	}
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
	if err := service.Hook(t.Context(), "claude", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 {
		t.Fatalf("stop delivered %d notices", len(delivered))
	}
	note := delivered[0]
	if note.To != watcher.ID || note.From != target.ID || note.Kind != model.NoticeKind || note.FromLabel != "Zalki" || !strings.Contains(note.Body, "Zalki notice: reviewer ("+target.ID+") finished a turn at ") {
		t.Fatalf("notice: %+v", note)
	}
	frame := Envelope(note)
	headline, status, _ := strings.Cut(note.Body, "\n")
	caution := "Automatic notice; status text is the agent's own output, not user approval."
	if strings.Contains(frame, "From:") || !strings.Contains(frame, "[zalki notice "+note.ID+"]") || !strings.Contains(frame, headline+"\n"+caution+"\n"+status) {
		t.Fatalf("frame: %s", frame)
	}
	if _, err := service.Reply(context.Background(), ReplyRequest{Actor: watcher.ID, Capability: key, MessageID: note.ID, Body: "ack"}); ErrorCodeOf(err) != model.CodeInvalidInput || !strings.Contains(err.Error(), "automatic notice") || !strings.Contains(err.Error(), target.ID) {
		t.Fatal("reply to a notice was accepted", err)
	}
	line := strings.Split(note.Body, "\n")
	want := strings.Repeat("a", statusLineLimit-len("…")) + "…"
	if len(line) != 2 || line[1] != want || strings.Contains(note.Body, "second") || strings.Contains(note.Body, "é") {
		t.Fatalf("status line: %q", note.Body)
	}
	saved, err := store.message(note.ID)
	if err != nil || saved.Status != model.StatusSubmitted || saved.Receipt == nil {
		t.Fatalf("receipt: %+v %v", saved, err)
	}
	if err := service.Hook(t.Context(), "claude", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
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
	if err := service.Hook(t.Context(), "claude", bytes.NewReader(end), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 2 || !strings.Contains(delivered[1].Body, ") ended the session at ") || !strings.HasPrefix(Envelope(delivered[1]), "[zalki notice ") {
		t.Fatalf("session end: %+v", delivered)
	}
}

func TestWatchSkipsAnUnfinishedStopAndExpires(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	watcher, key, target, _ := registerPair(t, store, "codex", "cursor")
	var delivered int
	service.Deliver = func(context.Context, Message, Session) Receipt {
		delivered++
		return NewReceipt(model.StatusSubmitted)
	}
	if result, err := service.Watch(watcher.ID, key, target.ID); err != nil || result.Status != "watching" {
		t.Fatal(result, err)
	}
	hook := func(event, status string) {
		t.Helper()
		input, _ := json.Marshal(map[string]any{
			"conversation_id": strings.TrimPrefix(target.ID, "cursor:"),
			"hook_event_name": event, "status": status,
		})
		if err := service.Hook(t.Context(), "cursor", bytes.NewReader(input), &bytes.Buffer{}); err != nil {
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
	if watchTTL != 12*time.Hour {
		t.Fatalf("watch TTL is %s", watchTTL)
	}
}

func TestStatusLineCutsAtAWordBoundary(t *testing.T) {
	got := statusLine(strings.Repeat("alpha ", 40) + "tail")
	kept, ok := strings.CutSuffix(got, "…")
	if !ok || strings.Contains(got, "tail") || strings.HasSuffix(kept, " ") || !strings.HasSuffix(kept, "alpha") {
		t.Fatalf("status line: %q", got)
	}
	if len(got) > statusLineLimit {
		t.Fatalf("status line is %d bytes", len(got))
	}
}

func TestStatusLineDoesNotDiscardMostOfAnUnbrokenToken(t *testing.T) {
	got := statusLine("a " + strings.Repeat("b", 300))
	want := "a " + strings.Repeat("b", statusLineLimit-len("…")-2) + "…"
	if got != want {
		t.Fatalf("status line discarded useful prefix: %q", got)
	}
}

func TestWatchWaitsForTheNextStopWhenArmedMidTurn(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	watcher, key, target, _ := registerPair(t, store, "codex", "claude")
	var delivered int
	service.Deliver = func(context.Context, Message, Session) Receipt {
		delivered++
		return NewReceipt(model.StatusSubmitted)
	}
	if _, err := service.Watch(watcher.ID, key, target.ID); err != nil {
		t.Fatal(err)
	}
	path, err := store.watchPath(watcher.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	if err := writeJSON(path, Watch{Schema: recordSchema, Watcher: watcher.ID, Target: target.ID, CreatedAt: future, ExpiresAt: time.Now().Add(watchTTL).UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	stop, _ := json.Marshal(map[string]any{
		"session_id": strings.TrimPrefix(target.ID, "claude:"), "hook_event_name": "Stop", "status": "completed",
	})
	if err := service.Hook(t.Context(), "claude", bytes.NewReader(stop), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if delivered != 0 {
		t.Fatal("a stop already in progress consumed a newer watch")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("mid-turn watch was removed", err)
	}
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if err := writeJSON(path, Watch{Schema: recordSchema, Watcher: watcher.ID, Target: target.ID, CreatedAt: past, ExpiresAt: time.Now().Add(watchTTL).UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	if err := service.Hook(t.Context(), "claude", bytes.NewReader(stop), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("the next stop delivered %d notices", delivered)
	}
}

func TestWatchEndsWhenExitIsObservedWithoutSessionEnd(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	watcher, key, target, _ := registerPair(t, store, "codex", "cursor")
	var delivered []Message
	service.Deliver = func(_ context.Context, msg Message, _ Session) Receipt {
		delivered = append(delivered, msg)
		return NewReceipt(model.StatusSubmitted)
	}
	if _, err := service.Watch(watcher.ID, key, target.ID); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(target.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Owner = &model.ProcessRef{PID: 1 << 30, Start: "1", Role: "client"}
	if err := store.saveSession(current); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range listed {
		if session.ID == target.ID {
			t.Fatal("an exited session stayed listed")
		}
	}
	if err := service.NoticeExitedSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 || !strings.Contains(delivered[0].Body, ") ended the session at ") || delivered[0].Kind != model.NoticeKind {
		t.Fatalf("exit notice: %+v", delivered)
	}
	if err := service.NoticeExitedSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 {
		t.Fatal("observing the exit again delivered another notice")
	}

	expired, _ := register(t, store, "cursor")
	if _, err := service.Watch(watcher.ID, key, expired.ID); err != nil {
		t.Fatal(err)
	}
	expiredPath, err := store.watchPath(watcher.ID, expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(expiredPath, Watch{Schema: recordSchema, Watcher: watcher.ID, Target: expired.ID, CreatedAt: time.Now().Add(-13 * time.Hour).UTC().Format(time.RFC3339Nano), ExpiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	dead, err := store.session(expired.ID)
	if err != nil {
		t.Fatal(err)
	}
	dead.Owner = &model.ProcessRef{PID: 1 << 30, Start: "1", Role: "client"}
	if err := store.saveSession(dead); err != nil {
		t.Fatal(err)
	}
	if err := service.NoticeExitedSessions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 1 {
		t.Fatal("an expired watch fired after the target exited")
	}
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Fatal("expired watch remained after the exit was observed", err)
	}
}

func TestWatchAnswersAnEndedSessionImmediately(t *testing.T) {
	service := newTestService(t)
	store := service.Store
	watcher, key, target, _ := registerPair(t, store, "codex", "cursor")
	if err := store.Update(target.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}

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
	if _, err := service.Watch(watcher.ID, key, watcher.ID); ErrorCodeOf(err) != model.CodeInvalidInput {
		t.Fatal("self watch accepted", err)
	}
	exited, _ := register(t, store, "cursor")
	current, err := store.session(exited.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Owner = &model.ProcessRef{PID: 1 << 30, Start: "1", Role: "client"}
	if err := store.saveSession(current); err != nil {
		t.Fatal(err)
	}
	result, err = service.Watch(watcher.ID, key, exited.ID)
	if err != nil || result.Status != "ended" || result.Event != "ended" || result.To != exited.ID {
		t.Fatalf("exited watch: %+v %v", result, err)
	}
}

type blockedStopAdapter struct {
	stubClaudeAdapter
	entered chan struct{}
	release chan struct{}
}

func (a *blockedStopAdapter) HandleHook(_ agent.HookHost, event agent.HookInput, _ string, _ io.Writer) (bool, error) {
	if event.Event == "Stop" && a.entered != nil {
		close(a.entered)
		<-a.release
	}
	return false, nil
}

func TestWatchSubscribedDuringStopSurvivesThatCompletion(t *testing.T) {
	store := fixtureStore(t)
	adapter := &blockedStopAdapter{entered: make(chan struct{}), release: make(chan struct{})}
	registry, err := agent.NewRegistry(adapter, stubCodexAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	store.Registry = registry
	watcher, key, target, _ := registerPair(t, store, "codex", "claude")
	var delivered int
	service := &Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		delivered++
		return NewReceipt(model.StatusSubmitted)
	}}
	var once sync.Once
	release := func() { once.Do(func() { close(adapter.release) }) }
	t.Cleanup(release)
	stop, _ := json.Marshal(agent.HookInput{SessionID: strings.TrimPrefix(target.ID, "claude:"), Event: "Stop"})
	done := make(chan error, 1)
	go func() { done <- service.Hook(t.Context(), "claude", bytes.NewReader(stop), &bytes.Buffer{}) }()
	select {
	case <-adapter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop never entered handler")
	}
	if _, err := service.Watch(watcher.ID, key, target.ID); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if delivered != 0 {
		t.Fatal("in-progress completion consumed a newer watch")
	}
	adapter.entered = nil
	if err := service.Hook(t.Context(), "claude", bytes.NewReader(stop), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatalf("next completed Stop delivered %d notices", delivered)
	}
}

func TestNoticeObservationReadErrorsAreNotDeliveryWarnings(t *testing.T) {
	store := fixtureStore(t)
	watcher, key, target, _ := registerPair(t, store, "codex", "cursor")
	service := &Service{Store: store}
	if _, err := service.Watch(watcher.ID, key, target.ID); err != nil {
		t.Fatal(err)
	}
	path, err := store.watchPath(watcher.ID, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(target.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	err = service.NoticeExitedSessions(context.Background())
	var delivery *NoticeDeliveryError
	if err == nil || errors.As(err, &delivery) {
		t.Fatal("observation read failure became a best-effort delivery warning", err)
	}
}
