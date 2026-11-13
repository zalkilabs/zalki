//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// This fixture runs only ready-made native artifacts: no build descendants.
// Retain the original child unreaped until cleanup, use its os.Process handle,
// and never recover or signal a numeric process group after Wait.
func startPTYUpdate(t *testing.T, binary, state, term string) *ptySession {
	t.Helper()
	master, path, err := openTestPTY()
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), binary, "--state", state, "--log-level", "off", "plugins", "update", "demo")
	cmd.Env = append(os.Environ(), "TERM="+term, "HOME="+t.TempDir())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err = cmd.Start()
	_ = slave.Close()
	if err != nil {
		_ = master.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); _ = master.Close() })
	s := &ptySession{master: master, more: make(chan struct{}, 1)}
	go s.collect()
	return s
}

func TestPluginUpdateRealTerminalReview(t *testing.T) {
	binary := checkRealBinary(t)
	for _, mode := range []struct {
		term, answer string
		accept       bool
	}{{"xterm-256color", "y", true}, {"dumb", "yes\n", true}, {"xterm-256color", "n", false}, {"xterm-256color", "\x03", false}} {
		t.Run(mode.term+"-"+mode.answer, func(t *testing.T) {
			state, folder, old := installedUpdateFolder(t, "demo")
			changeUpdateFolder(t, folder)
			s := startPTYUpdate(t, binary, state, mode.term)
			s.answer(t, time.After(15*time.Second), [2]string{"Update with these choices?", mode.answer})
			if mode.accept {
				s.waitFor(t, "Updated demo:", time.After(15*time.Second))
				now := readInstallStatus(t, state, "demo")
				if now.Install.InstalledBy != "terminal" || now.Install.TreeSHA256 == old.Install.TreeSHA256 {
					t.Fatal("real terminal update lost proof or snapshot")
				}
			} else {
				s.waitFor(t, "Cancelled. The old version is still installed.", time.After(15*time.Second))
				if now := readInstallStatus(t, state, "demo"); now.Install.TreeSHA256 != old.Install.TreeSHA256 {
					t.Fatal("real terminal cancel changed installation")
				}
			}
			if strings.Count(s.output(), "Update with these choices?") != 1 {
				t.Fatal("update asked an extra question", s.output())
			}
		})
	}
}
