package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

type inboxReadCLIFixture struct {
	binary  string
	store   bus.Store
	key     string
	message model.Message
	status  bus.PluginFeedStatus
	marker  string
}

func newInboxReadCLIFixture(t *testing.T) inboxReadCLIFixture {
	t.Helper()
	binary := checkRealBinary(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	store, _ := pluginOwnerFixture(t)
	author, key, err := store.Register("cursor", "20000000-0000-4000-8000-000000000083", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	executable := filepath.Join(dir, "plugin")
	marker := filepath.Join(dir, "must-not-run")
	script := "#!/bin/sh\nprintf invoked > '" + strings.ReplaceAll(marker, "'", "'\\''") + "'\n"
	if err = os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	capabilityCLIOK(t, binary, store.Dir, "add", "external", "--exec", executable)
	capabilityCLIOK(t, binary, store.Dir, "configure", "external", "--session", author.ID)
	capabilityCLIOK(t, binary, store.Dir, "grant", "external", "--accept-from", author.ID, "--inbox-read")
	capabilityCLIOK(t, binary, store.Dir, "enable", "external")
	status, err := bus.NewPluginFeed(store, "external").PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	m, err := service.Send(context.Background(), bus.SendRequest{Actor: author.ID, Capability: key, To: "plugin:" + status.ID, MessageID: "30000000-0000-4000-8000-000000000083", Body: "PRIVATE-C1A2-CLI-BODY <>&\nsecret"})
	if err != nil {
		t.Fatal(err)
	}
	return inboxReadCLIFixture{binary, store, key, m, status, marker}
}
func (x inboxReadCLIFixture) run(t *testing.T, args ...string) ([]byte, []byte, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), x.binary, append([]string{"--state", x.store.Dir, "--log-level", "debug", "--log-file", "-", "plugins"}, args...)...)
	var out, diagnostic bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &diagnostic
	err := cmd.Run()
	return out.Bytes(), diagnostic.Bytes(), err
}
func TestPluginInboxReadActualCLIOutputPrivacyAndNonConsumption(t *testing.T) {
	x := newInboxReadCLIFixture(t)
	paths := []string{filepath.Join(x.store.Dir, "messages", x.message.ID+".json"), filepath.Join(x.store.Dir, "plugins", "external.data", "inbox.json"), filepath.Join(x.store.Dir, "plugins", "external.json")}
	before := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = string(raw)
	}
	out, diagnostic, err := x.run(t, "inbox-read", "external", "--message", x.message.ID)
	assertPluginReadCLIResult(t, out, diagnostic, &x.message)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Message struct {
			Generation string `json:"sender_generation"`
			Revision   string `json:"admission_revision"`
		} `json:"message"`
	}
	if json.Unmarshal(out, &result) != nil || result.Message.Generation == "" || result.Message.Revision != x.status.Capabilities.Revision || bytes.Contains(out, []byte(x.key)) {
		t.Fatal("CLI omitted provenance or exposed capability")
	}
	for path, want := range before {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != want {
			t.Fatal("CLI consumed message/index/quota or changed installation", err)
		}
	}
	if _, err = os.Stat(x.marker); !os.IsNotExist(err) {
		t.Fatal("owner read started the plugin", err)
	}
	assertInboxReadCLIPrivateState(t, x)
	out, diagnostic, err = x.run(t, "inbox-read", "external", "--message", x.message.ID)
	assertInboxReadCLIFailure(t, x, out, diagnostic, err)
	if !bytes.Contains(diagnostic, []byte("rate_limited")) {
		t.Fatal("new CLI process lost persistent throttle")
	}
	capabilityCLIOK(t, x.binary, x.store.Dir, "revoke", "external")
	out, diagnostic, err = x.run(t, "inbox-read", "external", "--message", x.message.ID)
	assertInboxReadCLIFailure(t, x, out, diagnostic, err)
}
func assertInboxReadCLIFailure(t *testing.T, x inboxReadCLIFixture, out, diagnostic []byte, err error) {
	t.Helper()
	if err == nil || len(out) != 0 || bytes.Contains(diagnostic, []byte("PRIVATE-C1A2-CLI-BODY")) || bytes.Contains(diagnostic, []byte(x.key)) {
		t.Fatal("CLI refusal leaked or replayed private output", len(out), err)
	}
}
func assertInboxReadCLIPrivateState(t *testing.T, x inboxReadCLIFixture) {
	t.Helper()
	err := filepath.WalkDir(x.store.Dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || path == filepath.Join(x.store.Dir, "messages", x.message.ID+".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte("PRIVATE-C1A2-CLI-BODY")) || bytes.Contains(raw, []byte(x.key)) {
			t.Error("private data escaped canonical message/output", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
func TestPluginInboxReadActualCLIRequiresExactScopeAndTrustNotice(t *testing.T) {
	x := newInboxReadCLIFixture(t)
	for _, args := range [][]string{{"inbox-read", "external"}, {"inbox-read", "--message", x.message.ID}, {"inbox-read", "external", "--message", "../message"}, {"inbox-read", "external", "--message", x.message.ID, "--session", x.message.From}, {"inbox-read", "external", "--message", x.message.ID, "--history"}, {"inbox-read", "external", "--message", x.message.ID, "extra"}} {
		out, diagnostic, err := x.run(t, args...)
		assertInboxReadCLIFailure(t, x, out, diagnostic, err)
	}
	out, diagnostic, err := x.run(t, "inbox-read", "--help")
	if err != nil || len(diagnostic) != 0 || !bytes.Contains(out, []byte(pluginTrustNotice)) || !bytes.Contains(out, []byte("does not start a plugin")) || !bytes.Contains(out, []byte("requires an explicit --inbox-read grant")) {
		t.Fatal("owner read help hid trust or permission scope", err)
	}
}

type inboxReadShortWriter struct{}

func (inboxReadShortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}
func TestPluginInboxReadCLIReportsShortWriteAfterAudit(t *testing.T) {
	x := newInboxReadCLIFixture(t)
	cmd := newRootCommand(t.TempDir())
	cmd.SetOut(inboxReadShortWriter{})
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--state", x.store.Dir, "plugins", "inbox-read", "external", "--message", x.message.ID})
	if err := cmd.Execute(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("partial owner body write reported success", err)
	}
	raw, err := os.ReadFile(filepath.Join(x.store.Dir, "plugin-inbox-read-audit.json"))
	if err != nil || !bytes.Contains(raw, []byte(`"outcome": "ok"`)) {
		t.Fatal("body output was attempted before audit", err)
	}
}
