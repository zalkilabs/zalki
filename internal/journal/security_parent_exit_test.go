package journal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestSecurityLockReleaseAfterOwnerExit(t *testing.T) {
	if mode := os.Getenv("ZALKI_SECURITY_OWNER_MODE"); mode != "" {
		securityOwnerHelper(t, mode)
		return
	}
	path := filepath.Join(t.TempDir(), "lock")
	t.Cleanup(func() { _ = os.WriteFile(path+".release", nil, 0600) })
	owner := securityOwnerCommand(t, path, "owner", -1)
	if output, err := owner.CombinedOutput(); err != nil {
		t.Fatalf("owner process failed: %s %v", output, err)
	}
	// CombinedOutput has reaped the owner; its unrelated child remains alive.
	var report struct{ Inherited bool }
	awaitSecurityFile(t, path+".ready", func(raw []byte) bool { return json.Unmarshal(raw, &report) == nil })
	if report.Inherited {
		t.Fatal("unrelated orphan retained the owner's exact lock inode")
	}
	if _, err := os.Stat(path + ".exited"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("orphan exited before the lock-release assertion")
	}
	fd, err := OpenLockFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer CloseLockFile(fd)
	if err := LockExclusive(fd, true); err != nil {
		t.Fatal("dead owner's unrelated child kept the lifetime lock", err)
	}
	if err := os.WriteFile(path+".release", nil, 0600); err != nil {
		t.Fatal(err)
	}
	awaitSecurityFile(t, path+".exited", func([]byte) bool { return true })
}

func securityOwnerCommand(t *testing.T, path, mode string, fd int) *exec.Cmd {
	t.Helper()
	command := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestSecurityLockReleaseAfterOwnerExit$")
	command.Env = append(os.Environ(), "ZALKI_SECURITY_OWNER_MODE="+mode, "ZALKI_SECURITY_OWNER_PATH="+path, "ZALKI_SECURITY_OWNER_FD="+strconv.Itoa(fd))
	return command
}

func securityOwnerHelper(t *testing.T, mode string) {
	t.Helper()
	path := os.Getenv("ZALKI_SECURITY_OWNER_PATH")
	if mode == "owner" {
		fd, err := OpenLockFile(path, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := LockExclusive(fd, true); err != nil {
			t.Fatal(err)
		}
		child := securityOwnerCommand(t, path, "orphan", fd)
		// No ExtraFiles and no inherited output pipe: the owner exits independently.
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		return
	}
	fd, err := strconv.Atoi(os.Getenv("ZALKI_SECURITY_OWNER_FD"))
	if err != nil {
		t.Fatal(err)
	}
	linked, err := Linked(fd, path)
	if err != nil && !errors.Is(err, syscall.EBADF) {
		t.Fatal(err)
	}
	raw, err := json.Marshal(struct{ Inherited bool }{linked})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".ready", raw, 0600); err != nil {
		t.Fatal(err)
	}
	// The helper has its own bound even if the test's owner is killed.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path + ".release"); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := os.WriteFile(path+".exited", nil, 0600); err != nil {
		t.Fatal(err)
	}
}

func awaitSecurityFile(t *testing.T, path string, ready func([]byte) bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && ready(raw) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("bounded descriptor helper did not reach its expected phase")
}
