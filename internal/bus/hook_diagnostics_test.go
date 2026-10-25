package bus

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
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
