package codex

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
)

// queueArgs reads the recorded Codex CLI contract shared with the core tests.
func queueArgs(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "bus", "testdata", "clients", "codex.json"))
	if err != nil {
		t.Fatal(err)
	}
	var contract struct {
		QueueArgs []string `json:"queue_args"`
	}
	if err := json.Unmarshal(data, &contract); err != nil || len(contract.QueueArgs) < 3 {
		t.Fatal("codex contract fixture has no queue arguments", err)
	}
	return contract.QueueArgs
}

func TestCodexTransportPinsUUIDAndNeverUsesShell(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "argv")
	program := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ZALKI_TEST_ARGS\"\n"
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ZALKI_TEST_ARGS", output)
	id := newTestID()
	msg := bus.Message{ID: newTestID(), From: "cursor:" + newTestID(), FromLabel: "Cursor", To: "codex:" + id, Body: "literal $(not-a-command) `also-literal`"}
	receipt := queueCodex(context.Background(), msg, bus.Session{ID: msg.To, Kind: "codex"})
	if receipt.Status != "submitted" {
		t.Fatalf("%+v", receipt)
	}
	args, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	contract := queueArgs(t)
	prefix := contract[0] + "\n" + contract[1] + "\n" + id + "\n" + contract[2] + "\n"
	if !strings.HasPrefix(string(args), prefix) || !strings.Contains(string(args), msg.Body) {
		t.Fatal(string(args))
	}
}

func TestCodexQueueUsesRecipientProfile(t *testing.T) {
	bin, recipient := t.TempDir(), t.TempDir()
	output := filepath.Join(bin, "profile")
	script := "#!/bin/sh\nprintf '%s' \"$CODEX_HOME\" > " + install.ShellQuote(output) + "\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CODEX_HOME", filepath.Join(bin, "sender-profile"))
	receipt := queueCodex(context.Background(), bus.Message{ID: newTestID(), Body: "profile test"}, bus.Session{Kind: "codex", ID: "codex:" + newTestID(), NativeConfigDir: recipient})
	data, err := os.ReadFile(output)
	if err != nil || string(data) != recipient || receipt.Status != bus.StatusSubmitted {
		t.Fatal("queue inherited sender profile", string(data), receipt, err)
	}
}
