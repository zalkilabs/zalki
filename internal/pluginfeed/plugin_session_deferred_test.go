package pluginfeed

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func sessionQuotaSnapshot(t *testing.T, s bus.Store) map[string]string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(s.Dir, "flood", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rows[filepath.Base(path)] = string(raw)
	}
	return rows
}

func TestSessionControlApprovalReleaseRechecksOriginalAdmission(t *testing.T) {
	t.Run("allowed", func(t *testing.T) { assertSessionApprovalRelease(t, false) })
	t.Run("muted", func(t *testing.T) { assertSessionApprovalRelease(t, true) })
}

func assertSessionApprovalRelease(t *testing.T, muted bool) {
	t.Helper()
	x := newPluginSendFixture(t)
	native, key := register(t, x.store, "codex")
	if err := x.f.GrantPluginAccess(PluginAccess{Status: true, Send: []string{native.ID}}); err != nil {
		t.Fatal(err)
	}
	status, err := x.f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	service := x.boundService(status)
	deliveries := 0
	service.Deliver = func(_ context.Context, _ model.Message, _ model.Session) model.Receipt {
		deliveries++
		return model.NewReceipt(model.StatusSubmitted)
	}
	if err := x.store.Update(native.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	message, err := service.SendFromPlugin(t.Context(), bus.PluginSendRequest{Installation: status.ID, Name: status.Name, ClientMessageID: "held-native", To: native.ID, Body: "held plugin private body"})
	if err != nil || !message.Held {
		t.Fatal("held admission positive", err)
	}
	quota := sessionQuotaSnapshot(t, x.store)
	if muted {
		if err := bus.SetSessionPlugin(x.store, native.ID, key, status.Name, true); err != nil {
			t.Fatal(err)
		}
	}
	input := `{"session_id":"` + native.NativeID + `","cwd":"/workspace","hook_event_name":"UserPromptSubmit"}`
	var out bytes.Buffer
	if err := service.Hook(t.Context(), "codex", bytes.NewBufferString(input), &out); err != nil {
		t.Fatal(err)
	}
	expected := 1
	if muted {
		expected = 0
	}
	if deliveries != expected || bytes.Contains(out.Bytes(), []byte(message.Body)) {
		t.Fatal("Z6_APPROVAL_RELEASE_FINAL_CURRENT_POLICY")
	}
	assertApprovalHistoryAndQuota(t, x.store, native, key, muted, quota)
}

func assertApprovalHistoryAndQuota(t *testing.T, store bus.Store, native model.Session, key string, muted bool, quota map[string]string) {
	t.Helper()
	if muted {
		page, err := store.ReadMessages(bus.MessageQuery{Actor: native.ID, Capability: key, History: true, IncludeBody: true})
		if err != nil || len(page.Messages) != 0 {
			t.Fatal("Z6_REFUSED_APPROVAL_CANNOT_LEAK_VIA_HISTORY", err)
		}
	}
	if !reflect.DeepEqual(quota, sessionQuotaSnapshot(t, store)) {
		t.Fatal("Z6_APPROVAL_FINAL_CHECK_NO_SECOND_QUOTA")
	}
}

func sessionControlEndpoint(t *testing.T) *model.Endpoint {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "z6-listener-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	endpoint, err := agent.InspectEndpoint(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return endpoint
}

func TestSessionControlListenerRechecksOriginalAdmission(t *testing.T) {
	x := newPluginSendFixture(t)
	message, err := x.send("listener-pending", x.granted.ID, "pending plugin private body")
	if err != nil {
		t.Fatal(err)
	}
	lease, err := x.store.AcquireListenerLease(x.granted.ID, x.key)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close(nil)
	if err := lease.Arm(sessionControlEndpoint(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.ValidateDelivery(message.ID); err != nil {
		t.Fatal("listener allowed-admission positive", err)
	}
	quota := sessionQuotaSnapshot(t, x.store)
	if err := bus.SetSessionPlugin(x.store, x.granted.ID, x.key, x.status.Name, true); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.ValidateDelivery(message.ID); err == nil {
		t.Fatal("Z6_LISTENER_FINAL_CURRENT_POLICY")
	}
	if !reflect.DeepEqual(quota, sessionQuotaSnapshot(t, x.store)) {
		t.Fatal("Z6_LISTENER_FINAL_CHECK_NO_SECOND_QUOTA")
	}
}
