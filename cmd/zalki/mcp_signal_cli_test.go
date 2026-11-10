package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMCPSignalActualCLI(t *testing.T) {
	binary := checkRealBinary(t)
	for _, test := range []struct {
		name   string
		signal os.Signal
	}{
		{"SIGTERM", syscall.SIGTERM}, {"SIGINT", os.Interrupt}, {"SIGHUP", syscall.SIGHUP}, {"stdinEOF", nil},
	} {
		t.Run(test.name, func(t *testing.T) { runMCPSignalActualCLI(t, binary, test.signal) })
	}
}

func runMCPSignalActualCLI(t *testing.T, binary string, sig os.Signal) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--state", filepath.Join(t.TempDir(), "state"), "--log-level", "debug", "--log-file", "-", "mcp")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var diagnostic bytes.Buffer
	cmd.Stderr = &diagnostic
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	initializeSignalMCP(t, json.NewEncoder(input), output)
	if sig != nil {
		err = cmd.Process.Signal(sig)
	} else {
		err = input.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Wait(); err != nil {
		t.Fatalf("MCP stop exit: %v %s", err, diagnostic.String())
	}
	assertCleanMCPStop(t, diagnostic.Bytes())
}

func assertCleanMCPStop(t *testing.T, diagnostic []byte) {
	t.Helper()
	stopped := 0
	for _, line := range bytes.Split(bytes.TrimSpace(diagnostic), []byte{'\n'}) {
		var row struct {
			Level   string `json:"level"`
			Message string `json:"msg"`
		}
		if err := json.Unmarshal(line, &row); err != nil {
			t.Fatal("non-JSON stderr on clean MCP stop", string(line))
		}
		if row.Level == "ERROR" {
			t.Fatal("clean stop logged ERROR", string(line))
		}
		if row.Message == "mcp.stopped" && row.Level == "INFO" {
			stopped++
		}
	}
	if stopped != 1 || bytes.Contains(diagnostic, []byte("zalki: context canceled")) {
		t.Fatalf("missing clean stop diagnostic: %s", diagnostic)
	}
}

func TestMCPActualCLIRealErrorStillFails(t *testing.T) {
	binary := checkRealBinary(t)
	cmd := exec.CommandContext(t.Context(), binary, "--state", filepath.Join(t.TempDir(), "state"), "--log-level", "debug", "--log-file", "-", "mcp")
	// Reading a directory from stdin produces a transport error on Unix.
	input, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	cmd.Stdin = input
	var output, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &diagnostic
	err = cmd.Run()
	if err == nil || cmd.ProcessState.ExitCode() != 1 || !strings.Contains(diagnostic.String(), `"msg":"command.failed"`) || !strings.Contains(diagnostic.String(), `"level":"ERROR"`) {
		t.Fatalf("real MCP error reported clean: %v %s", err, diagnostic.String())
	}
}
