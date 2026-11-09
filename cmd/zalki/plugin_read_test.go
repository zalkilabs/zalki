package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func pluginReadOwnerFixture(t *testing.T) (string, bus.Store, model.Message) {
	t.Helper()
	binary := checkRealBinary(t)
	store, session := pluginOwnerFixture(t)
	executable := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	capabilityCLIOK(t, binary, store.Dir, "add", "external", "--exec", executable)
	capabilityCLIOK(t, binary, store.Dir, "configure", "external", "--session", session.ID)
	m := model.Message{Schema: 1, ID: "10000000-0000-4000-8000-000000000002", From: session.ID, To: session.ID, TargetGeneration: session.Generation, Body: "PRIVATE-B1-CLI-BODY <>&\nsecret", CreatedAt: "2000-01-01T00:00:00Z", AcknowledgedAt: "2000-01-02T00:00:00Z", Status: model.StatusSubmitted}
	raw, err := journal.EncodeJSON(m, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(store.Dir, "messages", m.ID+".json"), raw); err != nil {
		t.Fatal(err)
	}
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--read", session.ID)
	capabilityCLIOK(t, binary, store.Dir, "enable", "external")
	return binary, store, m
}

func TestPluginReadOwnerCLIOutputPrivacyAndReplay(t *testing.T) {
	binary, store, m := pluginReadOwnerFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--state", store.Dir, "--log-level", "debug", "plugins", "read", "external", "--session", m.To, "--message", m.ID)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatal("owner read failed", stderr.String(), err)
	}
	assertPluginReadCLIResult(t, stdout.Bytes(), stderr.Bytes(), &m)
	if err := filepath.WalkDir(store.Dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || path == filepath.Join(store.Dir, "messages", m.ID+".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if bytes.Contains(raw, []byte("PRIVATE-B1-CLI-BODY")) {
			t.Error("body leaked outside canonical message", path)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Rate state survives an entirely new CLI process. A terminal revoke wins
	// over this rate window and never replays the previously returned buffer.
	if raw, err := pluginOwnerCLI(t, binary, store.Dir, "read", "external", "--session", m.To, "--message", m.ID); err == nil || bytes.Contains(raw, []byte("PRIVATE-B1-CLI-BODY")) || !bytes.Contains(raw, []byte("rate_limited")) {
		t.Fatal("CLI retry bypassed rate or replayed body", len(raw), err)
	}
	capabilityCLIOK(t, binary, store.Dir, "revoke", "external")
	if raw, err := pluginOwnerCLI(t, binary, store.Dir, "read", "external", "--session", m.To, "--message", m.ID); err == nil || bytes.Contains(raw, []byte("PRIVATE-B1-CLI-BODY")) {
		t.Fatal("CLI revoke replayed body", len(raw), err)
	}
}

func TestPluginReadOwnerCLIRequiredScopeAndHelp(t *testing.T) {
	binary, store, m := pluginReadOwnerFixture(t)
	for _, args := range [][]string{{"read", "external"}, {"read", "external", "--session", m.To}, {"read", "external", "--message", m.ID}, {"read", "external", "--session", "display-name", "--message", m.ID}, {"read", "external", "--session", m.To, "--message", "../message"}, {"read", "external", "--session", m.To, "--message", m.ID, "--history"}} {
		raw, err := pluginOwnerCLI(t, binary, store.Dir, args...)
		if err == nil || bytes.Contains(raw, []byte("PRIVATE-B1-CLI-BODY")) {
			t.Fatal("CLI accepted invalid read scope", args, len(raw), err)
		}
	}
	raw, err := pluginOwnerCLI(t, binary, store.Dir, "read", "--help")
	if err != nil || !strings.Contains(string(raw), "The plugin executable runs as the owner; grants filter only what Zalki exposes.") || !strings.Contains(string(raw), "does not start a plugin or prove process liveness") {
		t.Fatal("help hid owner/host limits", string(raw), err)
	}
	capabilityCLIOK(t, binary, store.Dir, "disable", "external")
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--send", m.To, "--inbox-read", "--accept-from", m.From)
	capabilityCLIOK(t, binary, store.Dir, "enable", "external")
	raw, err = pluginOwnerCLI(t, binary, store.Dir, "read", "external", "--session", m.To, "--message", m.ID)
	if err == nil || bytes.Contains(raw, []byte("PRIVATE-B1-CLI-BODY")) {
		t.Fatal("other permissions implied CLI read", len(raw), err)
	}
}

func assertPluginReadCLIResult(t *testing.T, stdout, stderr []byte, m *model.Message) {
	t.Helper()
	var result struct {
		Request string `json:"request_id"`
		Code    string `json:"code"`
		Message *struct {
			ID   string  `json:"id"`
			Body *string `json:"body"`
		} `json:"message"`
	}
	if json.Unmarshal(stdout, &result) != nil || result.Code != "ok" || result.Message == nil || result.Message.ID != m.ID || result.Message.Body == nil || *result.Message.Body != m.Body || len(stderr) != 0 {
		t.Fatal("owner read output/privacy contract failed", len(stdout), len(stderr))
	}
}
