package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestPluginReadActualCLIWithholdsEchoedBodyStderr(t *testing.T) {
	binary := checkRealBinary(t)
	store, session := pluginOwnerFixture(t)
	result := filepath.Join(t.TempDir(), "result.json")
	executable := filepath.Join(t.TempDir(), "reader")
	script := `#!/bin/sh
printf '{"hello":{"wire":"zalki-plugin/1","requires":["feed","read"]}}\n'
IFS= read -r host_hello || exit 2
IFS= read -r hello_ok || exit 2
IFS= read -r offer || exit 3
id=${offer#*\"id\":\"}
id=${id%%\"*}
printf '{"ack":"%s"}\n' "$id"
printf '{"op":"read","request_id":"r1","args":{"session":"%s","message":"%s"}}\n' "$2" "$3"
IFS= read -r result || exit 4
printf '%s\n' "$result" > "$1"
printf '%s\n' "$result" >&2
exit 17
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	const id = "20000000-0000-4000-8000-000000000001"
	const body = "B2-CLI-PRIVATE-BODY-MUST-NOT-REACH-HOST-STDERR"
	m := model.Message{Schema: 1, ID: id, From: session.ID, To: session.ID, TargetGeneration: session.Generation, Body: body, CreatedAt: "2000-01-01T00:00:00Z", AcknowledgedAt: "2000-01-02T00:00:00Z", Status: model.StatusSubmitted}
	raw, err := journal.EncodeJSON(m, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(store.Dir, "messages", id+".json"), raw); err != nil {
		t.Fatal(err)
	}
	capabilityCLIOK(t, binary, store.Dir, "add", "reader", "--exec", executable, "--", result, session.ID, id)
	capabilityCLIOK(t, binary, store.Dir, "configure", "reader", "--session", session.ID)
	capabilityCLIOK(t, binary, store.Dir, "grant", "reader", "--read", session.ID)
	capabilityCLIOK(t, binary, store.Dir, "enable", "reader")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--state", store.Dir, "--log-level", "debug", "plugins", "run", "reader")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err == nil {
		t.Fatal("nonzero child exit was accepted")
	}
	if bytes.Contains(stdout.Bytes(), []byte(body)) || bytes.Contains(stderr.Bytes(), []byte(body)) {
		t.Fatal("actual CLI exposed child-echoed body")
	}
	assertCLIReadReceipt(t, result, body)
	audit, err := os.ReadFile(filepath.Join(store.Dir, "plugin-read-audit.json"))
	if err != nil || bytes.Contains(audit, []byte(body)) {
		t.Fatal("CLI audit absent or leaked", err)
	}
}

func assertCLIReadReceipt(t *testing.T, path, body string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	var frame struct {
		Reply struct {
			Op     string `json:"op"`
			Code   string `json:"code"`
			Result struct {
				Message *struct {
					Body string `json:"body"`
				} `json:"message"`
			} `json:"result"`
		} `json:"reply"`
	}
	result := &frame.Reply
	if err != nil || json.Unmarshal(raw, &frame) != nil || result.Op != "read" || result.Code != "ok" || result.Result.Message == nil || result.Result.Message.Body != body {
		t.Fatal("actual child did not receive the granted body", err)
	}
}
