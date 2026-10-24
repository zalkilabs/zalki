package bus

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func logRows(t *testing.T, output *bytes.Buffer) []map[string]any {
	t.Helper()
	var rows []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(output.Bytes()))
	for scanner.Scan() {
		var row map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			t.Fatal("invalid structured log", err)
		}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestMessageLogsCorrelateWithoutExposingPrivateContent(t *testing.T) {
	store := fixtureStore(t)
	var output bytes.Buffer
	store.Logger = slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
	private := "private-content-sentinel"
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), private))
	from, key := register(t, store, "codex")
	to, toKey := register(t, store, "codex")
	if err := store.hookName(from.ID, private); err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, Deliver: func(context.Context, Message, Session) Receipt {
		return Receipt{Status: StatusSubmitted, Output: private + key}
	}}
	msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: private})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: msg.ID, Body: private}); err != nil {
		t.Fatal(err)
	}
	reply, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: toKey, MessageID: msg.ID, Body: private})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ack(from.ID, key, reply.ID); err != nil {
		t.Fatal(err)
	}
	// Malformed identities and errors can contain arbitrary peer/credential text.
	if _, err := service.Send(context.Background(), SendRequest{Actor: key, Capability: key, To: private, MessageID: key, Body: private + key}); err == nil {
		t.Fatal("credential leak accepted")
	}
	service.ObserveTool("send_message", from.ID, msg.ID, 1, fmt.Errorf("%s %s", key, private))
	service.Deliver = func(context.Context, Message, Session) Receipt {
		return Receipt{Status: StatusNotSent, Error: private + key, Code: ErrorCode(key), Output: private + key, cause: fmt.Errorf("%s: %w", private, syscall.ECONNREFUSED)}
	}
	if _, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: key, To: to.ID, MessageID: newID(), Body: private}); err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(HookInput{SessionID: newID(), CWD: "/fixture/" + private, Event: "sessionStart"})
	var hookOutput bytes.Buffer
	if err := service.Hook("cursor", bytes.NewReader(input), &hookOutput); err != nil {
		t.Fatal(err)
	}
	var startup struct {
		Env map[string]string `json:"env"`
	}
	if err := json.Unmarshal(hookOutput.Bytes(), &startup); err != nil || startup.Env["ZALKI_CAPABILITY"] == "" {
		t.Fatal("hook did not issue its private context", err)
	}
	for _, secret := range []string{private, key, toKey, from.KeyHash, to.KeyHash, startup.Env["ZALKI_CAPABILITY"], "BEGIN PEER BODY", "Shell background task"} {
		if strings.Contains(output.String(), secret) {
			t.Fatal("private content reached structured diagnostics")
		}
	}
	counts := map[string]int{}
	correlated := false
	classified := false
	for _, row := range logRows(t, &output) {
		event := row["msg"].(string)
		counts[event]++
		if row["message_id"] == msg.ID && row["actor"] == from.ID && row["recipient"] == to.ID {
			correlated = true
		}
		if row["error_code"] == key {
			t.Fatal("unknown error code bypassed the metadata allowlist")
		}
		if row["msg"] == "transport.completed" && row["error_kind"] == "connection_refused" {
			classified = true
		}
	}
	for _, event := range []string{"session.registered", "message.reserved", "transport.started", "transport.completed", "message.acknowledged", "reply.committed", "message.reused", "message.send_failed", "hook.completed", "tool.send_message"} {
		if counts[event] == 0 {
			t.Fatal("missing diagnostic event", event)
		}
	}
	if !correlated || !classified || counts["message.acknowledged"] != 2 || counts["transport.completed"] != 3 {
		t.Fatal("diagnostics lost correlation or treated a retry as delivery", counts)
	}
}

func TestRecoveryLoggingKeepsUncertainReplyUnsubmitted(t *testing.T) {
	store := fixtureStore(t)
	var output bytes.Buffer
	store.Logger = slog.New(slog.NewJSONHandler(&output, nil))
	from, _ := register(t, store, "codex")
	to, key := register(t, store, "cursor")
	parent := Message{ID: newID(), From: from.ID, To: to.ID, Body: "private-recovery-body", Status: StatusSubmitted}
	if err := store.saveMessage(parent); err != nil {
		t.Fatal(err)
	}
	var reply Message
	if err := store.locked(func() error {
		var err error
		reply, _, _, err = store.prepareMessage(SendRequest{Actor: to.ID, Capability: key, To: from.ID, MessageID: replyID(to.ID, parent.ID, "private-reply-body"), Body: "private-reply-body", ReplyTo: parent.ID}, false)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(store.Dir, "transactions")
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	tx := replyTransaction{Schema: 1, ParentID: parent.ID, AcknowledgedAt: timestamp(), Reply: reply}
	if err := writeJSON(filepath.Join(dir, reply.ID+".json"), tx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, row := range logRows(t, &output) {
		if row["msg"] == "reply.recovered" {
			matched = row["level"] == "WARN" && row["message_id"] == reply.ID && row["reply_to"] == parent.ID && row["status"] == string(StatusAttempting)
		}
		if row["msg"] == "transport.completed" {
			t.Fatal("recovery submitted a transport")
		}
	}
	if !matched || strings.Contains(output.String(), "private-recovery-body") || strings.Contains(output.String(), "private-reply-body") {
		t.Fatal("recovery diagnostic missing or unsafe")
	}
}
