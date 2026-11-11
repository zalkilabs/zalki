//go:build darwin || linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ptyShell is the owner's shell on the terminal: it runs zalki, reports its
// exit code, then reads the next line typed on the terminal and echoes it. The
// INT trap keeps the shell alive when Ctrl-C reaches its process group, as an
// interactive shell would; zalki itself starts with the default action.
const ptyShell = `trap : INT; "$0" "$@"; echo "zalki exited $?"; IFS= read -r line; echo "shell got: $line"`

// stillWaitingWindow is how long a test lets zalki handle a first cancel
// before it checks that zalki still waits at its point of no return.
const stillWaitingWindow = 500 * time.Millisecond

// ptySession runs the real binary under ptyShell on a pseudo-terminal.
type ptySession struct {
	master *os.File
	mu     sync.Mutex
	text   strings.Builder
	more   chan struct{}
}

func (s *ptySession) collect() {
	buf := make([]byte, 4096)
	for {
		n, err := s.master.Read(buf)
		s.mu.Lock()
		s.text.Write(buf[:n])
		s.mu.Unlock()
		select {
		case s.more <- struct{}{}:
		default:
		}
		if err != nil {
			close(s.more)
			return
		}
	}
}

func (s *ptySession) output() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

// waitFor blocks until the output contains want; it fails after the deadline.
func (s *ptySession) waitFor(t *testing.T, want string, deadline <-chan time.Time) {
	t.Helper()
	for !strings.Contains(s.output(), want) {
		select {
		case _, ok := <-s.more:
			if !ok && !strings.Contains(s.output(), want) {
				t.Fatalf("output ended without %q:\n%s", want, s.output())
			}
		case <-deadline:
			t.Fatalf("no %q in time:\n%s", want, s.output())
		}
	}
}

func (s *ptySession) typeText(t *testing.T, text string) {
	t.Helper()
	if _, err := s.master.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// answer waits for each prompt in turn and types its answer.
func (s *ptySession) answer(t *testing.T, deadline <-chan time.Time, steps ...[2]string) {
	t.Helper()
	for _, step := range steps {
		s.waitFor(t, step[0], deadline)
		s.typeText(t, step[1])
	}
}

// assertExitThenShellGetsInput checks that zalki exits with code within 5 s,
// and that a line typed afterwards reaches the shell, not zalki.
func (s *ptySession) assertExitThenShellGetsInput(t *testing.T, code string) {
	t.Helper()
	s.waitFor(t, "zalki exited "+code, time.After(5*time.Second))
	s.typeText(t, "next command\n")
	s.waitFor(t, "shell got: next command", time.After(5*time.Second))
}

// Ctrl-C on a real terminal must end the install at once and install nothing.
// In line mode (TERM=dumb) the terminal turns it into a real SIGINT; on the
// checkbox screen (raw mode) it is a key. Native and Go plugins.
func TestPluginInstallRealTerminalCtrlC(t *testing.T) {
	binary := checkRealBinary(t)
	for _, c := range []struct {
		name, term string
		goTool     bool
		steps      [][2]string
		ctrlCAt    string
	}{
		{"line mode, native, at the final question", "dumb", false, [][2]string{{"Sessions for changes", "1\n"}, {"Sessions for read", "\n"}, {"own inbox?", "n\n"}}, "Install and enable"},
		{"line mode, go, at the first prompt", "dumb", true, nil, "Sessions for changes"},
		{"checkbox screen, native", "xterm-256color", false, nil, "Arrows move"},
	} {
		t.Run(c.name, func(t *testing.T) {
			store, _ := pluginOwnerFixture(t)
			folder := nativeInstallFolder(t)
			if c.goTool {
				folder = fakeGoFolder(t, `printf built > plugin; chmod 700 plugin`)
			}
			session := startPTYInstall(t, binary, store.Dir, folder, c.term)
			deadline := time.After(20 * time.Second)
			session.answer(t, deadline, c.steps...)
			session.waitFor(t, c.ctrlCAt, deadline)
			session.typeText(t, "\x03")
			session.waitFor(t, "Cancelled. Nothing was installed.", deadline)
			session.assertExitThenShellGetsInput(t, "0")
			if out := session.output(); strings.Contains(out, "Installed and enabled") || strings.Contains(out, "Building...") {
				t.Fatalf("work went on after Ctrl-C:\n%s", out)
			}
			assertNothingInstalled(t, store.Dir)
		})
	}
}

// Ctrl-C while the build runs stops it at once; nothing is left behind.
func TestPluginInstallRealTerminalCtrlCDuringBuild(t *testing.T) {
	binary := checkRealBinary(t)
	store, _ := pluginOwnerFixture(t)
	session := startPTYInstall(t, binary, store.Dir, fakeGoFolder(t, `exec sleep 30`), "dumb")
	deadline := time.After(20 * time.Second)
	session.answer(t, deadline, [2]string{"Sessions for changes", "1\n"}, [2]string{"Sessions for read", "\n"}, [2]string{"Install and enable", "y\n"})
	session.waitFor(t, "Building...", deadline)
	session.typeText(t, "\x03")
	session.waitFor(t, "Cancelled. Nothing was installed.", deadline)
	session.assertExitThenShellGetsInput(t, "0")
	if strings.Contains(session.output(), "build failed") {
		t.Fatal(session.output())
	}
	assertNothingInstalled(t, store.Dir)
}

// At the point of no return (Commit waits for the Store lock) the first Ctrl-C
// cannot stop the install; the second one ends the process at once.
func TestPluginInstallRealTerminalSecondCtrlCKills(t *testing.T) {
	binary := checkRealBinary(t)
	store, _ := pluginOwnerFixture(t)
	session := startPTYInstall(t, binary, store.Dir, nativeInstallFolder(t), "dumb")
	deadline := time.After(20 * time.Second)
	session.answer(t, deadline, [2]string{"Sessions for changes", "1\n"}, [2]string{"Sessions for read", "\n"}, [2]string{"own inbox?", "n\n"})
	session.waitFor(t, "Install and enable", deadline)
	lock := holdStoreLock(t, store.Dir)
	defer lock.Close()
	session.typeText(t, "y\n")
	session.waitFor(t, "Enabling...", deadline)
	session.typeText(t, "\x03")
	// Give zalki time to handle the first Ctrl-C (it restores the default
	// action); it must still be waiting for the lock afterwards, as designed.
	wait := time.After(stillWaitingWindow)
	for waited := false; !waited; {
		select {
		case <-session.more:
		case <-wait:
			waited = true
		}
		if strings.Contains(session.output(), "zalki exited") {
			t.Fatalf("the first Ctrl-C ended the commit wait:\n%s", session.output())
		}
	}
	session.typeText(t, "\x03")
	session.assertExitThenShellGetsInput(t, "130") // 128 + SIGINT: killed by the default action
	if _, err := os.Stat(filepath.Join(store.Dir, "plugins", "demo.json")); err == nil {
		t.Fatal("a record was written although the lock was held")
	}
}

// holdStoreLock takes the Store's exclusive lock, as another zalki would.
func holdStoreLock(t *testing.T, state string) *os.File {
	t.Helper()
	file, err := os.OpenFile(filepath.Join(state, "lock"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	return file
}

// startPTYInstall starts ptyShell with the real binary on a new pseudo-terminal
// that is the shell's controlling terminal. A pty open error fails the test:
// only a missing ZALKI_TEST_BINARY may skip it. Cleanup always kills the
// session's process group and waits for it, so an early failure leaves no orphan.
func startPTYInstall(t *testing.T, binary, state, folder, term string) *ptySession {
	t.Helper()
	master, slavePath, err := openTestPTY()
	if err != nil {
		t.Fatal("cannot open a pseudo-terminal:", err)
	}
	t.Cleanup(func() { _ = master.Close() })
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", ptyShell, binary, "--state", state, "--log-level", "off", "plugins", "install", folder)
	cmd.Env = append(os.Environ(), "TERM="+term, "HOME="+t.TempDir())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	err = cmd.Start()
	_ = slave.Close()
	if err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // the shell's own session and group
		select {
		case <-waited:
		case <-time.After(10 * time.Second):
			t.Error("the shell and zalki were not reaped")
		}
	})
	session := &ptySession{master: master, more: make(chan struct{}, 1)}
	go session.collect()
	return session
}
