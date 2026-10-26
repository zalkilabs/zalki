package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"

	"testing"
)

func TestCursorWakeRejectsMisboundHints(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, toKey := register(t, store, "cursor")
	endpoint := &Endpoint{Path: "/fixture/socket"}
	state := cursorState(to)
	state.Wake = endpoint
	setCursorState(&to, state)
	if err := store.locked(func() error { return store.saveSession(to) }); err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(msg.ID, Receipt{Status: "pending_next_activity"}); err != nil {
		t.Fatal(err)
	}
	for _, hint := range []CursorWakeHint{
		{MessageID: msg.ID, Generation: "old-generation"},
		{MessageID: newID(), Generation: to.Generation},
		{MessageID: "\nZALKI_WAKE injected", Generation: to.Generation},
	} {
		client, server := net.Pipe()
		var out bytes.Buffer
		done := make(chan error, 1)
		go func() {
			defer server.Close()
			done <- store.consumeCursorHint(server, to.ID, to.Generation, endpoint, &out)
		}()
		if err := json.NewEncoder(client).Encode(hint); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err == nil || out.Len() != 0 {
			t.Fatalf("misbound hint produced model output: %q %v", out.String(), err)
		}
		client.Close()
	}
	if err := store.ListenCursor(context.Background(), to.ID, "wrong capability", &bytes.Buffer{}); err == nil {
		t.Fatal("listener accepted a different session capability")
	}
	if err := store.ListenCursor(context.Background(), from.ID, key, &bytes.Buffer{}); err == nil {
		t.Fatal("listener accepted a Codex identity")
	}
	if _, err := store.Ack(to.ID, toKey, msg.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCursorWakeOutputFailurePreservesCause(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, _ := register(t, store, "cursor")
	endpoint := &Endpoint{Path: "/fixture/socket"}
	state := cursorState(to)
	state.Wake = endpoint
	setCursorState(&to, state)
	if err := store.locked(func() error { return store.saveSession(to) }); err != nil {
		t.Fatal(err)
	}
	msg, _, _, err := store.ReserveMessage(SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Finish(msg.ID, Receipt{Status: StatusPendingActivity}); err != nil {
		t.Fatal(err)
	}
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- store.consumeCursorHint(server, to.ID, to.Generation, endpoint, failedRecoveryOutput{})
	}()
	if err := json.NewEncoder(client).Encode(CursorWakeHint{MessageID: msg.ID, Generation: to.Generation}); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, errCursorOutput) || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("wake output failure lost its classification or underlying cause: %v", err)
	}
}

func TestCursorPendingDoesNotReplayPreviousGeneration(t *testing.T) {
	store := fixtureStore(t)
	from, key := register(t, store, "codex")
	to, _ := register(t, store, "cursor")
	service := Service{Store: store}
	if _, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: "previous registration"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register("cursor", strings.TrimPrefix(to.ID, "cursor:"), to.CWD, nil); err != nil {
		t.Fatal(err)
	}
	if text, err := store.EmitPending(to.ID); err != nil || text != "" {
		t.Fatalf("replayed a message into a replacement registration: %q %v", text, err)
	}
}
