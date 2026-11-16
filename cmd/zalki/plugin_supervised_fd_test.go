package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// S26: under a real controller the plugin gets the read-only lease at FD 3
// (never the control pipe), no other descriptor, and only HOME and PATH.
func TestPluginSupervisedPluginFDsCLI(t *testing.T) {
	binary := os.Getenv("ZALKI_TEST_BINARY")
	if binary == "" {
		t.Skip("requires the locally built real zalki binary")
	}
	probe := buildSupervisedFDProbe(t)
	checkFDProbeDetectsExtraFiles(t, probe)
	t.Run("controller", func(t *testing.T) {
		checkSupervisedFDInheritance(t, binary, probe)
	})
	t.Run("inherited parent capabilities", func(t *testing.T) {
		checkSupervisedFDInheritance(t, binary, probe, supervisorFDInputs(t)...)
	})
}

func checkSupervisedFDInheritance(t *testing.T, binary, probe string, extraFiles ...*os.File) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	// Register before startSupervisor: its later owned shutdown cleanup joins
	// the host before these fixture-only logs are read, and TempDir runs last.
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for _, name := range []string{"demo.log.1", "demo.log"} {
			file, err := os.Open(filepath.Join(store.Dir, "plugins", name))
			if err != nil {
				t.Logf("descriptor fixture host log %s could not be read: %v", name, err)
				continue
			}
			raw, readErr := io.ReadAll(io.LimitReader(file, 64<<10))
			closeErr := file.Close()
			t.Logf("descriptor fixture host log %s (at most 64 KiB):\n%s", name, raw)
			if readErr != nil || closeErr != nil {
				t.Logf("descriptor fixture host log %s read/close errors: %v / %v", name, readErr, closeErr)
			}
		}
	})
	for _, args := range [][]string{{"add", "demo", "--exec", probe}, {"configure", "demo", "--scope", "all"}, {"grant", "demo", "--status"}, {"enable", "demo"}} {
		if output, err := pluginOwnerCLI(t, binary, store.Dir, args...); err != nil {
			t.Fatal(args, string(output), err)
		}
	}
	startSupervisor(t, binary, store.Dir, extraFiles...)
	report := awaitFDReport(t, store.Dir)
	var lease syscall.Stat_t
	if err := syscall.Stat(filepath.Join(store.Dir, "plugins", "demo.lock"), &lease); err != nil {
		t.Fatal(err)
	}
	if err := pluginFDReportProblem(report, &lease); err != nil {
		t.Fatal(err)
	}
}

// Seed known non-CLOEXEC capabilities at the controller's exec boundary.
// They must not reach the plugin through the supervised host. Keep
// an unseeded controller case too: padding this test input must not hide the
// original ambient-descriptor failure.
func supervisorFDInputs(t *testing.T) []*os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "parent-sentinel")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sentinel.Close() })
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = reader.Close()
		_ = writer.Close()
	})
	var regular, pipe syscall.Stat_t
	if err := syscall.Fstat(int(sentinel.Fd()), &regular); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Fstat(int(writer.Fd()), &pipe); err != nil {
		t.Fatal(err)
	}
	t.Logf("parent sentinel identities: FD 300 %d:%d; FD 301 %d:%d", statDevice(&regular), regular.Ino, statDevice(&pipe), pipe.Ino)
	files := make([]*os.File, 299)
	files[297], files[298] = sentinel, writer // FD 300 and FD 301 in the controller.
	return files
}

func pluginFDReportProblem(report []string, lease *syscall.Stat_t) error {
	want := fmt.Sprintf("fd3 %d:%d regular=true write_ebadf=true", statDevice(lease), lease.Ino)
	if len(report) == 0 || report[0] != want {
		return fmt.Errorf("the plugin's FD 3 is not the read-only lease: %q, want %q", report, want)
	}
	env := make([]string, 0, len(report)-1)
	for _, line := range report[1:] {
		if strings.HasPrefix(line, "open ") {
			return fmt.Errorf("a descriptor above FD 3 reached the plugin: %q", report)
		}
		env = append(env, strings.TrimPrefix(line, "env "))
	}
	slices.Sort(env)
	if !slices.Equal(env, []string{"HOME", "PATH"}) {
		return fmt.Errorf("the plugin's environment names are %q", env)
	}
	return nil
}

// The observed plugin is native: Go runtime/testing/race startup may open
// descriptors before a test function can inspect the inheritance boundary.
func buildSupervisedFDProbe(t *testing.T) string {
	t.Helper()
	probe := filepath.Join(t.TempDir(), "fd-probe")
	cmd := exec.CommandContext(t.Context(), "/usr/bin/cc", "-std=c11", "-O0", "-Wall", "-Wextra", "-Werror", "-o", probe, "testdata/fdprobe.c")
	cmd.Env = []string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build native descriptor probe: %v\n%s", err, out)
	}
	compiled, err := os.ReadFile(probe)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native FD probe SHA256: %x", sha256.Sum256(compiled))
	return probe
}

// The same oracle must reject an inherited regular file, writable pipe and
// high descriptor. These fixture-owned children only wait for stdin EOF.
func checkFDProbeDetectsExtraFiles(t *testing.T, probe string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sentinel")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sentinel, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer sentinel.Close()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	var lease syscall.Stat_t
	if err := syscall.Fstat(int(sentinel.Fd()), &lease); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		file *os.File
		fd   int
	}{
		{"regular file", sentinel, 4},
		{"writable pipe", writer, 4},
		{"high writable pipe", writer, 300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.CommandContext(t.Context(), probe)
			cmd.Dir, cmd.Env = dir, []string{"HOME=" + dir, "PATH=/usr/bin:/bin:/usr/sbin:/sbin"}
			cmd.ExtraFiles = make([]*os.File, tc.fd-2)
			cmd.ExtraFiles[0], cmd.ExtraFiles[tc.fd-3] = sentinel, tc.file
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("descriptor probe negative control: %v\n%s", err, out)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "fd-report"))
			if err != nil {
				t.Fatal(err)
			}
			var extra syscall.Stat_t
			if err := syscall.Fstat(int(tc.file.Fd()), &extra); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("open %d %d:%d ", tc.fd, statDevice(&extra), extra.Ino)
			report := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			problem := pluginFDReportProblem(report, &lease)
			if !strings.Contains(string(raw), want) || problem == nil || !strings.HasPrefix(problem.Error(), "a descriptor above FD 3 reached the plugin:") {
				t.Fatalf("descriptor probe missed the inherited %s at FD %d: %q %v", tc.name, tc.fd, report, problem)
			}
		})
	}
}

func awaitFDReport(t *testing.T, state string) []string {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		paths, _ := filepath.Glob(filepath.Join(state, "plugins", "*", "fd-report"))
		if len(paths) == 1 {
			if raw, err := os.ReadFile(paths[0]); err == nil && strings.HasSuffix(string(raw), "\n") {
				return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the plugin never wrote its descriptor report")
	return nil
}
