package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestHookLargePayloadActualCLI(t *testing.T) {
	binary := checkRealBinary(t)
	store, _ := pluginOwnerFixture(t)
	const native = "00000000-0000-4000-8000-000000000082"
	target, _, err := store.Register("claude", native, t.TempDir(), &model.Endpoint{Path: "/fixture/not-a-real-socket"})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Update(target.ID, model.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	input := fmt.Sprintf(`{"hook_event_name":"PostToolUse","session_id":%q,"cwd":"/fixture/workspace","tool_response":%q}`, native, strings.Repeat("x", 131000))
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--state", store.Dir, "--log-level", "off", "hook", "--agent", "claude")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	var out, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err = io.WriteString(stdin, input); err != nil {
		_ = cmd.Wait()
		t.Fatal(err)
	}
	// Keep stdin open: success must not depend on an EOF from the caller.
	if err = cmd.Wait(); err != nil || out.Len() != 0 || diagnostic.Len() != 0 {
		t.Fatalf("hook exit: %v stdout=%q stderr=%q", err, out.String(), diagnostic.String())
	}
	sessions, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ID == target.ID {
			if session.State != model.StateBusy {
				t.Fatalf("event was not applied: %s", session.State)
			}
			return
		}
	}
	t.Fatal("target missing after hook")
}
