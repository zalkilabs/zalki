package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func TestHookFailuresIdentifyStageWithoutLoggingInput(t *testing.T) {
	const private = "private-hook-content-sentinel"
	id := newID()
	input := func(event string, status any, cwd string) string {
		data, err := json.Marshal(map[string]any{
			"session_id": id, "hook_event_name": event, "status": status,
			"cwd": cwd, "prompt": private, "session_title": private,
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, test := range []struct {
		name, input, stage, kind string
		correlated               bool
	}{
		{"empty", "", "decode", "eof", false},
		{"truncated", "{", "decode", "unexpected_eof", false},
		{"wrong_type", input("SessionStart", map[string]string{"secret": private}, "/fixture/workspace"), "decode", "json_type_mismatch", true},
		{"unknown_event", input(private, "", "/fixture/workspace"), "event", "other", false},
		{"registration", input("SessionStart", "", private), "register", "other", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("ZALKI_CAPTURE_CONTRACTS", "")
			store := fixtureStore(t)
			var diagnostic, output bytes.Buffer
			store.Logger = slog.New(slog.NewJSONHandler(&diagnostic, nil))
			service := Service{Store: store}
			if err := service.Hook("codex", strings.NewReader(test.input), &output); err == nil {
				t.Fatal("invalid hook input was accepted")
			}
			rows := logRows(t, &diagnostic)
			if len(rows) != 1 {
				t.Fatalf("expected one failure diagnostic, got %d", len(rows))
			}
			row := rows[0]
			if row["msg"] != "hook.failed" || row["hook_stage"] != test.stage || row["error_kind"] != test.kind {
				t.Fatalf("wrong diagnostic: %v", row)
			}
			if test.correlated && (row["actor"] != "codex:"+id || row["hook_event"] != "SessionStart") {
				t.Fatalf("valid event identity was lost: %v", row)
			}
			if strings.Contains(diagnostic.String(), private) || strings.Contains(diagnostic.String(), "/fixture/workspace") {
				t.Fatal("hook content or path reached diagnostics")
			}
			files, err := filepath.Glob(filepath.Join(store.Dir, "sessions", "*.json"))
			if err != nil || len(files) != 0 || output.Len() != 0 {
				t.Fatal("failed hook registered a session or issued startup context")
			}
		})
	}
}

func TestHookDiagnosticStageRejectsArbitraryText(t *testing.T) {
	store := fixtureStore(t)
	var output bytes.Buffer
	store.Logger = slog.New(slog.NewJSONHandler(&output, nil))
	store.logEvent(slog.LevelWarn, "hook.failed", eventData{HookStage: "private-stage-sentinel"})
	rows := logRows(t, &output)
	if len(rows) != 1 || rows[0]["hook_stage"] != nil || strings.Contains(output.String(), "private-stage-sentinel") {
		t.Fatal("unrecognized hook stage bypassed the metadata allowlist")
	}
}

// Fail at adapter boundaries to verify that extracted stages retain diagnostics
// and stop before later registration or startup output can occur.
type hookStageFailureAdapter struct {
	stubCodexAdapter
	stage string
}

func (a hookStageFailureAdapter) NormalizeHook(event agent.HookInput) (agent.HookInput, error) {
	if a.stage == "normalize" {
		return agent.HookInput{}, errors.New("private-hook-stage-sentinel")
	}
	return event, nil
}

func (a hookStageFailureAdapter) HandleHook(_ agent.HookHost, _ agent.HookInput, _ string, _ io.Writer) (bool, error) {
	if a.stage == "handler" {
		return false, errors.New("private-hook-stage-sentinel")
	}
	return false, nil
}

func (a hookStageFailureAdapter) StartupOutput(startup agent.StartupContext) (any, error) {
	if a.stage == "startup" {
		return nil, errors.New("private-hook-stage-sentinel")
	}
	return a.stubCodexAdapter.StartupOutput(startup)
}

type closedHookWriter struct{}

func (closedHookWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestHookAdapterFailuresPreserveStageAndRegistrationBoundary(t *testing.T) {
	for _, stage := range []string{"normalize", "identity", "handler", "startup", "output"} {
		t.Run(stage, func(t *testing.T) {
			t.Setenv("ZALKI_CAPTURE_CONTRACTS", "")
			store := fixtureStore(t)
			var err error
			store.Registry, err = NewRegistry(hookStageFailureAdapter{stage: stage})
			if err != nil {
				t.Fatal(err)
			}
			var diagnostic bytes.Buffer
			store.Logger = slog.New(slog.NewJSONHandler(&diagnostic, nil))
			id := newID()
			if stage == "identity" {
				id = ""
			}
			input, err := json.Marshal(agent.HookInput{SessionID: id, CWD: "/workspace", Event: "SessionStart"})
			if err != nil {
				t.Fatal(err)
			}
			service := Service{Store: store}
			if err := service.Hook("codex", bytes.NewReader(input), closedHookWriter{}); err == nil {
				t.Fatal("failed boundary was accepted")
			}
			failures := 0
			for _, row := range logRows(t, &diagnostic) {
				if row["msg"] != "hook.failed" {
					continue
				}
				failures++
				if row["hook_stage"] != stage || row["hook_event"] != "SessionStart" {
					t.Fatalf("failure lost its stage or native event correlation: %v", row)
				}
				if (stage == "handler" || stage == "startup" || stage == "output") && row["actor"] != "codex:"+id {
					t.Fatalf("failure lost its validated actor: %v", row)
				}
			}
			if failures != 1 || strings.Contains(diagnostic.String(), "private-hook-stage-sentinel") || strings.Contains(diagnostic.String(), "private capability") {
				t.Fatal("failure diagnostic count or content changed")
			}
			files, err := filepath.Glob(filepath.Join(store.Dir, "sessions", "*.json"))
			wantRecords := 0
			if stage == "startup" || stage == "output" {
				wantRecords = 1
			}
			if err != nil || len(files) != wantRecords {
				t.Fatalf("registration crossed its failure boundary: %d want %d: %v", len(files), wantRecords, err)
			}
		})
	}
}
