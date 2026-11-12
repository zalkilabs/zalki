package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

func TestPluginInboxActualCLIStoredOnlyAndFailurePrivacy(t *testing.T) {
	binary := checkRealBinary(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	store, _ := pluginOwnerFixture(t)
	author, key, err := store.Register("cursor", "00000000-0000-4000-8000-000000000072", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(t.TempDir(), "plugin")
	if err = os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	capabilityCLIOK(t, binary, store.Dir, "add", "inbox-fixture", "--exec", executable)
	capabilityCLIOK(t, binary, store.Dir, "configure", "inbox-fixture", "--session", author.ID)
	capabilityCLIOK(t, binary, store.Dir, "grant", "inbox-fixture", "--receive", author.ID)
	capabilityCLIOK(t, binary, store.Dir, "enable", "inbox-fixture")
	status, err := pluginfeed.NewPluginFeed(store, "inbox-fixture").PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	const body = "C1-CLI-PRIVATE-STORAGE-BODY"
	input := map[string]string{"actor": author.ID, "to": "plugin:" + status.ID, "message_id": "30000000-0000-4000-8000-000000000072", "body": body}
	send := func() ([]byte, []byte, error) {
		raw, _ := json.Marshal(input)
		cmd := exec.CommandContext(t.Context(), binary, "--state", store.Dir, "--log-level", "debug", "send")
		cmd.Env = append(os.Environ(), "ZALKI_CAPABILITY="+key)
		cmd.Stdin = bytes.NewReader(raw)
		var out, diagnostic bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &diagnostic
		err := cmd.Run()
		return out.Bytes(), diagnostic.Bytes(), err
	}
	out, diagnostic, err := send()
	checkCLIStoredInbox(t, out, diagnostic, err, body, key, input["to"], status.ID)
	input["body"] = "C1-CLI-CONFLICTING-PRIVATE-BODY"
	out, diagnostic, err = send()
	if err == nil || len(out) != 0 || bytes.Contains(diagnostic, []byte(body)) || bytes.Contains(diagnostic, []byte(input["body"])) || bytes.Contains(diagnostic, []byte(key)) {
		t.Fatal("actual CLI failure leaked body or exposed success")
	}
	help := exec.CommandContext(t.Context(), binary, "send", "--help")
	raw, err := help.Output()
	if err != nil || !strings.Contains(string(raw), "Stored, not delivered; owner reading is a later command. Not acknowledged.") {
		t.Fatal("CLI storage help promised delivery", err)
	}
	raw, err = os.ReadFile(filepath.Join(store.Dir, "logs", "zalki.jsonl"))
	if err != nil || bytes.Contains(raw, []byte(body)) || bytes.Contains(raw, []byte(input["body"])) || bytes.Contains(raw, []byte(key)) {
		t.Fatal("CLI log missing or exposed private data", err)
	}
}

func checkCLIStoredInbox(t *testing.T, out, diagnostic []byte, err error, body, key, to, installation string) {
	t.Helper()
	var m model.Message
	if err != nil || json.Unmarshal(out, &m) != nil {
		t.Fatal("actual CLI did not store unacknowledged record", err)
	}
	if m.Receipt == nil {
		t.Fatal("missing stored receipt")
	}
	if !reflect.DeepEqual([]any{m.Status, m.AcknowledgedAt, m.To, m.TargetGeneration, m.Receipt.Output}, []any{model.StatusSubmitted, "", to, installation, ""}) {
		t.Fatal("stored receipt promised delivery")
	}
	if bytes.Contains(diagnostic, []byte(body)) || bytes.Contains(out, []byte(key)) || bytes.Contains(diagnostic, []byte(key)) {
		t.Fatal("actual CLI leaked credential/body in diagnostics")
	}
}
