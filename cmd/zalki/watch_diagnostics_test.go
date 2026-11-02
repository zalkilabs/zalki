package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestDiagnosticsKeepOutputWhenACompletionWatcherIsUnreachable(t *testing.T) {
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(name, t.TempDir())
	}
	for _, command := range []string{"list", "prune"} {
		t.Run(command, func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(home, "state")
			store := bus.Store{Dir: state, Registry: adapters.Builtin()}
			watcher, key, err := store.Register("cursor", "00000000-0000-4000-8000-000000000031", "/fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			target, _, err := store.Register("cursor", "00000000-0000-4000-8000-000000000032", "/fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			live, _, err := store.Register("cursor", "00000000-0000-4000-8000-000000000033", "/fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := (&bus.Service{Store: store}).Watch(watcher.ID, key, target.ID); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{watcher.ID, target.ID} {
				if err := store.Update(id, bus.StateRetired, true); err != nil {
					t.Fatal(err)
				}
			}
			out, diagnostic, err := execute(t, home, "", command, "--state", state)
			if err != nil || strings.Count(diagnostic, "\n") != 1 || !strings.Contains(diagnostic, "completion notice") {
				t.Fatalf("notice failure hid output or failed command: %v stderr=%q", err, diagnostic)
			}
			if !json.Valid([]byte(out)) {
				t.Fatalf("lost structured output: %q", out)
			}
			if command == "list" && !strings.Contains(out, live.ID) {
				t.Fatal("live session disappeared from output")
			}
			// Observation consumes the watch even when its recipient cannot receive.
			if err := (&bus.Service{Store: store}).NoticeExitedSessions(context.Background()); err != nil {
				t.Fatal("notice repeated", err)
			}
		})
	}
}
