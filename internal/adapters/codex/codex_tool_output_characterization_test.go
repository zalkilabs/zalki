package codex

import (
	"context"
	"errors"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Pin the existing protocol, outcome and support-cache boundaries before extraction.
func TestSubmitCodexToolOutputOutcomes(t *testing.T) {
	for _, test := range []struct {
		mode    string
		joined  bool
		err     error
		support codexSupport
	}{
		{"idle", false, nil, codexSupportVerified},
		{"busy", true, nil, codexSupportVerified},
		{"paged", false, nil, codexSupportVerified},
		{"not-loaded", false, errCodexNotLoaded, codexSupportUnknown},
		{"identity", false, errCodexIdentity, codexSupportUnknown},
		{"home", false, errCodexUnsupported, codexSupportUnavailable},
		{"old", false, errCodexUnsupported, codexSupportUnavailable},
		{"approval", false, errCodexApproval, codexSupportUnknown},
		{"method-missing", false, errCodexUnsupported, codexSupportUnavailable},
		{"invalid-params", false, errCodexUnsupported, codexSupportUnavailable},
		{"internal", false, errCodexUncertain, codexSupportUnknown},
		{"malformed", false, errCodexUncertain, codexSupportUnknown},
		{"approval-rpc", false, errCodexApproval, codexSupportUnknown},
		{"unsupported", false, errCodexUnsupported, codexSupportUnknown},
		{"closed", false, errCodexUnavailable, codexSupportUnknown},
		{"hang", false, errCodexUncertain, codexSupportUnknown},
		{"dropped", false, errCodexUncertain, codexSupportUnknown},
		{"empty-turn", false, errCodexUncertain, codexSupportUnknown},
	} {
		t.Run(test.mode, func(t *testing.T) {
			checkCodexSubmission(t, test.mode, test.joined, test.err, test.support)
		})
	}
}

func checkCodexSubmission(t *testing.T, mode string, wantJoined bool, wantErr error, wantSupport codexSupport) {
	t.Helper()
	root, listener := listenCodexControl(t)
	defer listener.Close()
	endpoint, err := inspectCodexControl(root)
	if err != nil {
		t.Fatal(err)
	}
	thread, output := newTestID(), "exact tool output"
	calls := serveFakeCodex(t, listener, fakeCodex{Home: root, Thread: thread, Mode: mode})
	timeout := 2 * time.Second
	if mode == "hang" {
		timeout = 200 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	joined, err := submitCodexToolOutput(ctx, endpoint, root, thread, output)
	if joined != wantJoined || !errors.Is(err, wantErr) {
		t.Fatalf("joined=%v error=%v; want joined=%v error=%v", joined, err, wantJoined, wantErr)
	}
	if support := cachedCodexSupport(endpoint, root); support != wantSupport {
		t.Fatalf("support=%v, want %v", support, wantSupport)
	}
	checkCodexSubmissionTrace(t, receiveCodexSubmission(t, calls), codexSubmissionMethods(mode), thread, output)
}

func receiveCodexSubmission(t *testing.T, calls <-chan observedCodex) observedCodex {
	t.Helper()
	select {
	case seen := <-calls:
		return seen
	case <-time.After(3 * time.Second):
		t.Fatal("app-server exchange did not close")
		return observedCodex{}
	}
}

func codexSubmissionMethods(mode string) string {
	switch mode {
	case "unsupported", "closed":
		return ""
	case "old", "home":
		return "initialize"
	case "not-loaded":
		return "initialize,initialized,thread/loaded/list"
	case "identity", "approval":
		return "initialize,initialized,thread/loaded/list,thread/read"
	case "paged":
		return "initialize,initialized,thread/loaded/list,thread/loaded/list,thread/read,turn/start"
	default:
		return "initialize,initialized,thread/loaded/list,thread/read,turn/start"
	}
}

func checkCodexSubmissionTrace(t *testing.T, seen observedCodex, want, thread, output string) {
	t.Helper()
	if got := strings.Join(seen.methods, ","); got != want {
		t.Fatalf("methods=%q, want %q", got, want)
	}
	if len(seen.forbidden) != 0 {
		t.Fatalf("session-changing methods: %v", seen.forbidden)
	}
	if !strings.Contains(want, "turn/start") {
		return
	}
	params := map[string]any{
		"threadId": thread,
		"input":    []any{},
		"toolOutput": map[string]any{
			"name": "zalki", "namespace": "zalki", "output": output, "retain": true,
		},
	}
	if !reflect.DeepEqual(seen.turn, params) {
		t.Fatalf("turn/start parameters=%#v, want %#v", seen.turn, params)
	}
}

func TestSubmitCodexToolOutputRechecksSocketIdentity(t *testing.T) {
	for _, method := range []string{"initialize", "thread/read"} {
		t.Run(method, func(t *testing.T) {
			checkCodexSocketReplacement(t, method)
		})
	}
}

func checkCodexSocketReplacement(t *testing.T, method string) {
	t.Helper()
	root, listener := listenCodexControl(t)
	defer listener.Close()
	endpoint, err := inspectCodexControl(root)
	if err != nil {
		t.Fatal(err)
	}
	changed := make(chan error, 1)
	spec := fakeCodex{Home: root, Thread: newTestID(), Mode: "busy", BeforeResponse: func(seen string) {
		if seen == method {
			changed <- replaceCodexSubmissionSocket(t, endpoint.Path)
		}
	}}
	calls := serveFakeCodex(t, listener, spec)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	joined, err := submitCodexToolOutput(ctx, endpoint, root, spec.Thread, "tool output")
	if joined || !errors.Is(err, errCodexUnavailable) {
		t.Fatalf("replacement at %s: joined=%v error=%v", method, joined, err)
	}
	checkCodexReplacementResult(t, receiveCodexSubmission(t, calls), changed, method)
	if got := cachedCodexSupport(endpoint, root); got != codexSupportUnknown {
		t.Fatalf("replacement changed support cache to %v", got)
	}
}

func replaceCodexSubmissionSocket(t *testing.T, path string) error {
	t.Helper()
	if err := os.Rename(path, path+".prior"); err != nil {
		return err
	}
	replacement, err := (&net.ListenConfig{}).Listen(t.Context(), "unix", path)
	if err != nil {
		return err
	}
	t.Cleanup(func() { _ = replacement.Close() })
	return os.Chmod(path, 0600)
}

func checkCodexReplacementResult(t *testing.T, seen observedCodex, changed <-chan error, method string) {
	t.Helper()
	select {
	case err := <-changed:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("fixture did not replace the endpoint")
	}
	for _, got := range seen.methods {
		if got == "turn/start" || method == "initialize" && got == "thread/loaded/list" {
			t.Fatalf("replacement at %s allowed subsequent method %s", method, got)
		}
	}
}
