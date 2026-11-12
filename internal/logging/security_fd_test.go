package logging

import (
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
)

func securityFD(t *testing.T, fd int) {
	t.Helper()
	if fd >= 1024 {
		t.Fatal("fixture fd exceeds child identity scan range")
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("opened descriptor lacks CLOEXEC: flags=%d err=%v", flags, err)
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSecurityFDChild$")
	command.Env = append(os.Environ(), "ZALKI_SECURITY_FD_DEV="+strconv.FormatUint(uint64(st.Dev), 10), "ZALKI_SECURITY_FD_INO="+strconv.FormatUint(st.Ino, 10))
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("unrelated exec inherited descriptor identity: %s %v", out, err)
	}
}

func TestSecurityFDChild(t *testing.T) {
	dev := os.Getenv("ZALKI_SECURITY_FD_DEV")
	if dev == "" {
		return
	}
	inode := os.Getenv("ZALKI_SECURITY_FD_INO")
	for fd := 3; fd < 1024; fd++ {
		var st unix.Stat_t
		if unix.Fstat(fd, &st) == nil && strconv.FormatUint(uint64(st.Dev), 10) == dev && strconv.FormatUint(st.Ino, 10) == inode {
			t.Fatal("unrelated exec retained exact opened inode")
		}
	}
}

func TestSecurityLogCloseOnExec(t *testing.T) {
	for _, name := range []string{"log", "log.lock", "log.1"} {
		file, err := privateLogFile(filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		securityFD(t, int(file.Fd()))
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
