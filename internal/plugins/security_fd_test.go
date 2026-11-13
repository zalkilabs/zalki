package plugins

import (
	"errors"
	"fmt"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
	"golang.org/x/sys/unix"
	"io"
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

func TestSecurityManifestCloseOnExec(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, installManifestFile), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	observed := false
	_, err := readInstallManifestObserved(dir, func(file *os.File) { observed = true; securityFD(t, int(file.Fd())) })
	if err != nil || !observed {
		t.Fatal("manifest observer not executed", err)
	}
}

func TestSecurityExplicitFD3TransferAndLifetime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lease")
	lease, err := pluginfeed.OpenPluginLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	var info unix.Stat_t
	if err := unix.Fstat(int(lease.Fd()), &info); err != nil {
		t.Fatal(err)
	}
	c, err := launchChild(t.Context(), os.Args[0], identityOf(t, os.Args[0]), []string{"-test.run=^TestSecurityFD3Child$"}, dir, dir, lease)
	if err != nil {
		t.Fatal(err)
	}
	defer c.stop(nil)
	line, err := c.reader.ReadString('\n')
	want := strconv.FormatUint(uint64(info.Dev), 10) + ":" + strconv.FormatUint(info.Ino, 10) + "\n"
	if err != nil || line != want {
		t.Fatalf("explicit child FD3 lost read-only lease identity: %q %v", line, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := pluginfeed.OpenPluginLease(path); err == nil {
		_ = other.Close()
		t.Fatal("child FD3 did not retain parent lease")
	}
	if err := c.stop(nil); err != nil {
		t.Fatal(err)
	}
	other, err := pluginfeed.OpenPluginLease(path)
	if err != nil {
		t.Fatal("child exit failed to release lease", err)
	}
	_ = other.Close()
}

func TestSecurityFD3Child(t *testing.T) {
	if len(os.Args) != 2 || os.Args[1] != "-test.run=^TestSecurityFD3Child$" {
		return
	}
	var info unix.Stat_t
	if err := unix.Fstat(3, &info); err != nil {
		t.Fatal("explicit lease FD3 absent", err)
	}
	if _, err := unix.Write(3, []byte("x")); !errors.Is(err, unix.EBADF) {
		t.Fatal("explicit FD3 was writable", err)
	}
	fmt.Printf("%d:%d\n", uint64(info.Dev), info.Ino)
	_, _ = io.Copy(io.Discard, os.Stdin)
}
