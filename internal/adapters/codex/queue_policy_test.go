package codex

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestCodexQueuePolicyDefaultRefusesAndOptInPreservesEnvelope(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "argv")
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nprintf '%s' \"$5\" > \"$ZALKI_TEST_ARGS\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("ZALKI_TEST_ARGS", output)
	target := model.Session{Kind: "codex", NativeID: newTestID(), NativeConfigDir: t.TempDir()}
	msg := model.Message{ID: newTestID(), Body: "full body [/zalki]\u200B"}
	for _, path := range []string{"/selected/settings.json", "/selected/%s%v.json", "/line\n\u202E\u200B\xff.json", "/" + strings.Repeat("界", 400), "/" + strings.Repeat("👋", 400)} {
		checkQueueRefusal(t, path, output, msg, target)
	}
	receipt := New(true, "fixture.json").Deliver(t.Context(), msg, target)
	captured, err := os.ReadFile(output)
	if err != nil || receipt.Status != model.StatusSubmitted || !model.MatchesEnvelope(msg, string(captured)) || !strings.Contains(string(captured), `\u005B/zalki]`) {
		t.Fatal("opt-in lost complete nonce envelope", receipt, err)
	}
	caps := New(true, "fixture.json").ReceiveCapabilities(target)
	health, _ := New(true, "fixture.json").Health(nil, target)
	if !caps.IdleWake || caps.Delivery != "native_queue_next_turn" || health != "native_queue_only" {
		t.Fatal("opt-in capabilities lost queue")
	}
}

func TestCodexQueueOffKeepsVerifiedAppServerPrimary(t *testing.T) {
	for _, mode := range []string{"idle", "busy", "approval", "dropped"} {
		t.Run(mode, func(t *testing.T) {
			checkQueueOffPrimary(t, mode)
		})
	}
}

func checkQueueRefusal(t *testing.T, path, output string, msg model.Message, target model.Session) {
	t.Helper()
	receipt := New(false, path).Deliver(t.Context(), msg, target)
	if receipt.Status != model.StatusNotSent || model.ErrorCodeOf(receipt.Cause()) != model.CodeUnavailable || strings.ContainsAny(receipt.Error, "\n\r\u202e\u200b") || strings.Contains(receipt.Error, msg.Body) || len(receipt.Error) > 4096 {
		t.Fatalf("unsafe off-config receipt: %+v", receipt)
	}
	for _, part := range []string{"codex.allow_queue_fallback=true", "other local users", "process listings", "30 s", "Agents must not change this setting."} {
		if !strings.Contains(receipt.Error, part) {
			t.Fatalf("missing refusal remedy %q", part)
		}
	}
	shown := path
	if runes := []rune(path); len(runes) > 256 {
		shown = string(runes[:256]) + "…"
	}
	if !strings.Contains(receipt.Error, strconv.QuoteToASCII(shown)) {
		t.Fatal("selected config path not safely quoted/capped")
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatal("queue executed while disabled")
	}
	caps := New(false, path).ReceiveCapabilities(target)
	health, _ := New(false, path).Health(nil, target)
	if caps.IdleWake || caps.Delivery != "app_server_queue_off_config" || health != "app_server_required_queue_off_config" || caps.Validate() != nil {
		t.Fatal("off-config capabilities were optimistic or invalid", caps, health)
	}
}

func checkQueueOffPrimary(t *testing.T, mode string) {
	t.Helper()
	root, listener := listenCodexControl(t)
	defer listener.Close()
	thread := newTestID()
	calls := serveFakeCodex(t, listener, fakeCodex{Home: root, Thread: thread, Mode: mode})
	marker := queueSpy(t)
	target := model.Session{Kind: "codex", NativeID: thread, NativeConfigDir: root}
	msg := model.Message{ID: newTestID(), Body: "primary"}
	receipt := New(false, "fixture.json").Deliver(context.Background(), msg, target)
	seen := <-calls
	if queueMarked(t, marker) {
		t.Fatal("queue executed while off")
	}
	switch mode {
	case "idle", "busy":
		if receipt.Status != model.StatusSubmitted || seen.turn == nil {
			t.Fatal("off-config blocked primary", receipt)
		}
		caps := New(false, "fixture.json").ReceiveCapabilities(target)
		if !caps.IdleWake || caps.Delivery != "app_server_tool_output" || caps.BusyDelivery != model.DeliveryBetweenTools {
			t.Fatal("verified app-server was not advertised", caps)
		}
	case "approval":
		if receipt.Status != model.StatusApprovalBlocked {
			t.Fatal("approval was not preserved", receipt)
		}
	case "dropped":
		if receipt.Status != model.StatusUnknownWrite {
			t.Fatal("uncertain write was not preserved", receipt)
		}
	}
}

func TestCodexQueueOffUnverifiedSocketHealth(t *testing.T) {
	root, listener := listenCodexControl(t)
	defer listener.Close()
	target := model.Session{Kind: "codex", NativeConfigDir: root}
	adapter := New(false, "fixture.json")
	caps := adapter.ReceiveCapabilities(target)
	health, _ := adapter.Health(nil, target)
	if caps.IdleWake || caps.Delivery != "app_server_unverified_queue_off_config" || health != "app_server_socket_present_queue_off_config" || caps.Validate() != nil {
		t.Fatal("unverified socket concealed disabled queue policy", caps, health)
	}
}
