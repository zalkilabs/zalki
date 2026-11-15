package codex

import (
	"context"
	"github.com/zalkilabs/zalki/internal/model"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexBoundProfileNeverUsesSenderRPC(t *testing.T) {
	for _, root := range []string{"", "relative"} {
		for _, allow := range []bool{false, true} {
			t.Run(root+map[bool]string{false: "/off", true: "/on"}[allow], func(t *testing.T) { checkBoundProfileRPC(t, root, allow) })
		}
	}
}
func checkBoundProfileRPC(t *testing.T, bad string, allow bool) {
	t.Helper()
	sender, listener := listenCodexControl(t)
	defer listener.Close()
	t.Setenv("CODEX_HOME", sender)
	id := newTestID()
	calls := serveFakeCodex(t, listener, fakeCodex{Home: sender, Thread: id, Mode: "idle"})
	marker := queueSpy(t)
	target := model.Session{Kind: "codex", ID: "codex:" + id, NativeID: id, NativeConfigDir: bad}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	receipt := New(allow, "fixture").Deliver(ctx, model.Message{ID: newTestID(), To: target.ID, Body: "private fixture"}, target)
	if receipt.Status == model.StatusSubmitted {
		select {
		case seen := <-calls:
			if seen.turn != nil {
				t.Fatal("FOREIGN_PROFILE_RPC_USED")
			}
		case <-ctx.Done():
			t.Fatal("foreign RPC observation did not join", ctx.Err())
		}
	}
	select {
	case seen := <-calls:
		if seen.turn != nil {
			t.Fatal("FOREIGN_PROFILE_RPC_USED")
		}
	default:
	}
	if receipt.Status != model.StatusNotSent || model.ErrorCodeOf(receipt.Cause()) != model.CodeUnavailable || queueMarked(t, marker) {
		t.Fatal("INVALID_BOUND_PROFILE_NOT_REFUSED", receipt)
	}
	if !strings.Contains(receipt.Error, "CODEX_HOME and HOME") || strings.Contains(receipt.Error, sender) {
		t.Fatal("BOUND_PROFILE_REFUSAL_LOST_VARIABLE_GUIDANCE", receipt.Error)
	}
	if caps := New(allow, "fixture").ReceiveCapabilities(target); caps.IdleWake || caps.Delivery == "app_server_tool_output" {
		t.Fatal("INVALID_BOUND_PROFILE_ADVERTISED", caps)
	}
}
func TestCodexBoundQueueRequiresCapturedProfile(t *testing.T) {
	for _, bad := range []string{"", "relative"} {
		t.Run(bad, func(t *testing.T) {
			t.Setenv("CODEX_HOME", t.TempDir())
			marker := queueSpy(t)
			target := model.Session{Kind: "codex", NativeID: newTestID(), NativeConfigDir: bad}
			receipt := queueCodex(t.Context(), model.Message{ID: newTestID(), Body: "fixture"}, target)
			if queueMarked(t, marker) {
				t.Fatal("FOREIGN_PROFILE_QUEUE_STARTED")
			}
			if receipt.Status != model.StatusNotSent || model.ErrorCodeOf(receipt.Cause()) != model.CodeUnavailable {
				t.Fatal("INVALID_BOUND_QUEUE_NOT_REFUSED", receipt)
			}
		})
	}
}
func TestCodexUnboundProfileProbeRemainsReadOnly(t *testing.T) {
	root, listener := listenCodexControl(t)
	defer listener.Close()
	t.Setenv("CODEX_HOME", root)
	endpoint, err := inspectCodexControl(root)
	if err != nil {
		t.Fatal(err)
	}
	rememberCodexSupport(endpoint, root, codexSupportVerified)
	before, err := os.Stat(filepath.Join(root, codexControlDirName))
	if err != nil {
		t.Fatal(err)
	}
	if caps := New(false, "fixture").ReceiveCapabilities(model.Session{}); !caps.IdleWake || caps.Delivery != "app_server_tool_output" {
		t.Fatal("UNBOUND_PROFILE_PROBE_LOST", caps)
	}
	checkCodexPartialBoundProfiles(t)
	if health, _ := New(false, "fixture").Health(nil, model.Session{}); health == "app_server_tool_output" {
		t.Fatal("UNBOUND_HEALTH_USED_SENDER_PROFILE")
	}
	after, err := os.Stat(filepath.Join(root, codexControlDirName))
	if err != nil || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("UNBOUND_PROBE_CHANGED_PROFILE", err)
	}
}

func checkCodexPartialBoundProfiles(t *testing.T) {
	t.Helper()
	for _, test := range []struct {
		name  string
		value model.Session
	}{
		{"kind", model.Session{Kind: "codex"}},
		{"generation", model.Session{Generation: "registered-generation"}},
		{"key", model.Session{KeyHash: "registered-key"}},
		{"endpoint", model.Session{Endpoint: &model.Endpoint{}}},
		{"schema", model.Session{Schema: 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := New(true, "fixture").ReceiveCapabilities(test.value); got.IdleWake || got.Delivery == "app_server_tool_output" {
				t.Fatal("PARTIAL_BOUND_PROFILE_BECAME_PROBE", got)
			}
		})
	}
}
