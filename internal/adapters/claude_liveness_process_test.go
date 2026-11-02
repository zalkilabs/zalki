package adapters_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestClaudeSocketHelper(t *testing.T) {
	if os.Getenv("ZALKI_SOCKET_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	path := filepath.Join(os.Getenv("ZALKI_SOCKET_DIR"), fmt.Sprintf("%d.sock", os.Getpid()))
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Println(path)
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeLivenessAfterKillAndCleanExit(t *testing.T) {
	for _, variable := range []string{"CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(variable, t.TempDir())
	}
	tools := t.TempDir()
	for _, name := range []string{"claude", "codex", "agent"} {
		if err := os.WriteFile(filepath.Join(tools, name), []byte("#!/bin/sh\nprintf '0.0.0\\n'\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", tools+":/usr/bin:/bin")
	for _, clean := range []bool{false, true} {
		t.Run(map[bool]string{false: "kill", true: "clean"}[clean], func(t *testing.T) {
			store := bus.Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: adapters.Builtin()}
			sender, key, err := store.Register("cursor", "00000000-0000-4000-8000-000000000001", "/workspace", nil)
			if err != nil {
				t.Fatal(err)
			}
			endpoint, stop := claudeSocket(t, clean)
			target, _, err := store.Register("claude", "00000000-0000-4000-8000-000000000002", "/workspace", endpoint)
			if err != nil {
				t.Fatal(err)
			}
			listed, err := store.List()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, session := range listed {
				if session.ID == target.ID {
					found = session.Liveness == "alive"
				}
			}
			if !found {
				t.Fatal("bound session socket did not establish alive liveness")
			}
			delivered := 0
			service := &bus.Service{Store: store, Deliver: func(context.Context, bus.Message, bus.Session) bus.Receipt {
				delivered++
				return bus.NewReceipt(bus.StatusSubmitted)
			}}
			stop()
			_, statErr := os.Stat(endpoint.Path)
			if !clean && statErr != nil {
				t.Fatal("kill removed the socket file", statErr)
			}
			if clean && !os.IsNotExist(statErr) {
				t.Fatal("clean exit left the socket file", statErr)
			}
			listed, err = store.List()
			if err != nil {
				t.Fatal(err)
			}
			for _, session := range listed {
				if session.ID == target.ID {
					t.Fatalf("exited session stayed listed as %s", session.Liveness)
				}
			}
			report, err := store.Diagnostics()
			if err != nil {
				t.Fatal(err)
			}
			exited := false
			for _, row := range report.Sessions {
				if row.ID == target.ID {
					exited = row.Liveness == "exited"
				}
			}
			if !exited {
				t.Fatal("diagnostics did not retain positive exit evidence")
			}
			_, err = service.Send(context.Background(), bus.SendRequest{
				Actor: sender.ID, Capability: key, To: target.ID,
				MessageID: "00000000-0000-4000-8000-000000000003", Body: "ping",
			})
			if err == nil || !strings.Contains(err.Error(), "exited") || delivered != 0 {
				t.Fatalf("send after exit reached transport or lost exit evidence: deliveries=%d err=%v", delivered, err)
			}
		})
	}
}

func claudeSocket(t *testing.T, clean bool) (*bus.Endpoint, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	dir, err := os.MkdirTemp("/private/tmp", "zalki-claude-exit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestClaudeSocketHelper$")
	cmd.Env = append(os.Environ(), "ZALKI_SOCKET_HELPER=1", "ZALKI_SOCKET_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		t.Helper()
		once.Do(func() {
			if clean {
				if _, err := io.WriteString(stdin, "exit\n"); err != nil {
					t.Error(err)
					_ = cmd.Process.Kill()
				}
			} else {
				_ = cmd.Process.Kill()
			}
			_ = stdin.Close()
			err := cmd.Wait()
			if clean && err != nil {
				t.Error("helper clean exit", err)
			}
		})
	}
	t.Cleanup(stop)
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := bus.InspectEndpoint(strings.TrimSpace(line))
	if err != nil {
		t.Fatal(err)
	}
	return endpoint, stop
}
