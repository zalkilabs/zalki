package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestDoctorInheritsCanceledContextAndKeepsAdvisoryReport(t *testing.T) {
	home := t.TempDir()
	marker := filepath.Join(home, "launched")
	script := "#!/bin/sh\nprintf yes > '" + marker + "'\nprintf 'client 0.163.0'\n"
	if err := os.WriteFile(filepath.Join(home, "codex"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home)
	for _, name := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
		t.Setenv(name, t.TempDir())
	}
	cmd := newRootCommand(home)
	var output, diagnostic bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&diagnostic)
	cmd.SetArgs([]string{"doctor"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := cmd.ExecuteContext(ctx); err != nil {
		t.Fatal("advisory cancellation failed doctor", err)
	}
	var report struct{ Agents []bus.AgentDiagnostic }
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || diagnostic.Len() != 0 {
		t.Fatal("doctor lost its JSON report", err)
	}
	for _, item := range report.Agents {
		if item.Kind == "codex" {
			if !item.Found || item.Compatibility != "version_unavailable" || item.Version != "" || item.RecoveryHint == "" {
				t.Fatal("doctor lost advisory fallback")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatal("doctor detached its probe from the command context", err)
			}
			return
		}
	}
	t.Fatal("doctor omitted installed client")
}
