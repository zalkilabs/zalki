package install_test

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
)

func TestInstallationProcessHelper(t *testing.T) {
	home := os.Getenv("ZALKI_INSTALL_TEST_HOME")
	if home == "" {
		return
	}
	mode := os.Getenv("ZALKI_INSTALL_TEST_MODE")
	if mode == "hold" {
		unlock, err := install.LockInstallation(home)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Println("ready")
		_, _ = bufio.NewReader(os.Stdin).ReadByte()
		unlock()
		os.Exit(0)
	}
	fmt.Println("ready")
	_, _ = bufio.NewReader(os.Stdin).ReadByte()
	_, err := install.Manage(bus.Configuration, install.Request{Action: "install", Binary: "/fixture/zalki", StateDir: os.Getenv("ZALKI_INSTALL_TEST_STATE"), HomeDir: home, Apply: true})
	result := struct {
		Error string `json:"error"`
	}{}
	if err != nil {
		result.Error = err.Error()
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		t.Fatal(err)
	}
	os.Exit(0)
}

func installationProcess(t *testing.T, home, state, mode string) (func(), func() string) {
	t.Helper()
	command := exec.Command(os.Args[0], "-test.run=^TestInstallationProcessHelper$")
	command.Env = append(os.Environ(), "ZALKI_INSTALL_TEST_HOME="+home, "ZALKI_INSTALL_TEST_STATE="+state, "ZALKI_INSTALL_TEST_MODE="+mode)
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })
	reader := bufio.NewReader(output)
	line, err := reader.ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("helper failed to reach barrier: %q %v %s", line, err, stderr.String())
	}
	return func() { _ = input.Close() }, func() string {
		var rest bytes.Buffer
		_, err := rest.ReadFrom(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err := command.Wait(); err != nil {
			t.Fatal(err, stderr.String())
		}
		return strings.TrimSpace(rest.String())
	}
}

func TestInstallationLockSerializesAcrossProcessesAndStates(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	release, wait := installationProcess(t, home, "", "hold")
	done := make(chan error, 1)
	go func() {
		_, err := install.Manage(bus.Configuration, install.Request{Action: "install", Binary: "/fixture/zalki", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true})
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("installer ignored another process's home lock: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatal("installer wrote settings before acquiring lock", err)
	}
	release()
	wait()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("installer did not continue after unlock")
	}
	if _, err := os.Stat(filepath.Join(home, ".zalki-install.lock")); err != nil {
		t.Fatal("lock inode was removed", err)
	}
}

func TestConcurrentInstallationsCannotMixOwnership(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	states := []string{filepath.Join(home, "first"), filepath.Join(home, "second")}
	releaseA, waitA := installationProcess(t, home, states[0], "install")
	releaseB, waitB := installationProcess(t, home, states[1], "install")
	releaseA()
	releaseB()
	results := []string{waitA(), waitB()}
	winner := -1
	for i, result := range results {
		var row struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal([]byte(result), &row); err != nil {
			t.Fatal(result, err)
		}
		if row.Error == "" {
			if winner >= 0 {
				t.Fatal("both states claimed the same settings")
			}
			winner = i
		}
	}
	if winner < 0 {
		t.Fatal("neither installer succeeded", results)
	}
	report, err := install.Manage(bus.Configuration, install.Request{Action: "install", Binary: "/fixture/zalki", StateDir: states[winner], HomeDir: home, Apply: false})
	if err != nil || len(report.Paths) != 0 {
		t.Fatal("concurrent installation left mixed settings", report, err)
	}
	if _, err := os.Stat(filepath.Join(states[1-winner], "installation.json")); !os.IsNotExist(err) {
		t.Fatal("loser acquired an ownership record", err)
	}
}

func TestInstallationRejectsUnsafeLock(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "target")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".zalki-install.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Manage(bus.Configuration, install.Request{Action: "install", Binary: "/fixture/zalki", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true}); err == nil {
		t.Fatal("followed an installation lock symlink")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "keep" {
		t.Fatal("lock target changed")
	}
}
