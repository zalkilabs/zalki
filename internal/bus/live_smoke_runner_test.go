package bus_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestLiveSmokeRunnerWithEmulatedClient(t *testing.T) {
	probeDir, err := os.MkdirTemp("/tmp", "zalki-smoke-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(probeDir)
	probe, err := net.Listen("unix", filepath.Join(probeDir, "probe.sock"))
	if errors.Is(err, syscall.EPERM) {
		t.Skip("execution sandbox blocks Unix socket bind")
	}
	if err != nil {
		t.Fatal(err)
	}
	probe.Close()
	workspace := t.TempDir()
	state := filepath.Join(t.TempDir(), "state")
	t.Setenv("CODEX_HOME", filepath.Join(workspace, "codex"))
	t.Setenv("CURSOR_CONFIG_DIR", filepath.Join(workspace, "cursor"))
	store := bus.Store{Dir: state, Registry: adapters.Builtin()}
	target, key, err := store.Register("codex", smokeID(), workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	program := "#!/bin/sh\nexec \"$ZALKI_TEST_PROCESS\" -test.run '^TestSmokeClientHelper$' -- \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "codex"), []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run", "^TestLiveClients$", "-test.v")
	command.Env = append(os.Environ(), "ZALKI_LIVE_SMOKE=1", "ZALKI_SMOKE_CASES=roundtrip", "ZALKI_SMOKE_STATE="+state,
		"ZALKI_SMOKE_WORKSPACE="+workspace, "ZALKI_SMOKE_TARGETS="+target.ID, "ZALKI_TEST_PROCESS="+binary,
		"ZALKI_TEST_CLIENT_HELPER=1", "ZALKI_TEST_CLIENT_KEY="+key, "ZALKI_TEST_CLIENT_ACTOR="+target.ID, "PATH="+binDir)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("emulated smoke run failed: %v\n%s", err, output)
	}
	files, err := filepath.Glob(filepath.Join(state, "smoke-reports", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatal("runner did not produce one report", err)
	}
	var report liveSmokeReport
	reportData, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(reportData, &report); err != nil || !report.Passed || len(report.Observations) != 1 || report.Observations[0].Status != "passed" {
		t.Fatal("runner did not verify the correlated reply", err)
	}
	raw, _ := os.ReadFile(files[0])
	if strings.Contains(string(raw), key) {
		t.Fatal("report included credentials")
	}
	sessions, err := store.List()
	if err != nil || len(sessions) != 1 || sessions[0].ID != target.ID {
		t.Fatal("runner did not retire only its own receiver", err)
	}
}

func TestSmokeClientHelper(t *testing.T) {
	if os.Getenv("ZALKI_TEST_CLIENT_HELPER") != "1" {
		t.Skip("subprocess fixture")
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		t.Fatal("missing native arguments")
	}
	args = args[1:]
	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("codex-cli 0.163.0")
		return
	}
	if len(args) != 5 || args[0] != "queue" || args[1] != "--thread" || args[3] != "--message" {
		t.Fatal("native queue contract changed")
	}
	prompt := args[4]
	line, _, _ := strings.Cut(prompt, "\n")
	id := strings.TrimSuffix(strings.TrimPrefix(line, "[zalki message "), "]")
	challenge := regexp.MustCompile(`ZALKI_SMOKE_[a-f0-9-]+`).FindString(prompt)
	if challenge == "" {
		t.Fatal("missing smoke challenge")
	}
	service := bus.Service{Store: bus.Store{Dir: os.Getenv("ZALKI_SMOKE_STATE"), Registry: adapters.Builtin()}}
	if _, err := service.Reply(context.Background(), bus.ReplyRequest{Actor: os.Getenv("ZALKI_TEST_CLIENT_ACTOR"), Capability: os.Getenv("ZALKI_TEST_CLIENT_KEY"), MessageID: id, Body: challenge + ": Transport acceptance alone cannot prove that a model read the request."}); err != nil {
		t.Fatal(err)
	}
}
