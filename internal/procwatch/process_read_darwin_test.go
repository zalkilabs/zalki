package procwatch

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"reflect"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// readCase is one injected answer of the row query and the probe.
type readCase struct {
	name     string
	pid      int
	row      *unix.KinfoProc
	rowErr   error
	probeErr error
	queries  int // wanted row queries
	probes   int // wanted probes
	absent   bool
	info     bool
	err      error // wanted error (nil: any non-absent error, or none with info)
}

func rowOf(pid int) *unix.KinfoProc {
	var row unix.KinfoProc
	row.Proc.P_pid = int32(pid)
	return &row
}

// ZAL-70: Read is one fixed-row query and at most one signal-0 probe; only
// the probe's ESRCH proves absence.
func TestProcwatchReadBounded(t *testing.T) {
	t.Run("matrix", checkReadMatrix)
	t.Run("selector", checkReadSelector)
	t.Run("real children", checkReadRealChildren)
}

func checkReadMatrix(t *testing.T) {
	const pid = 4242
	cases := []readCase{
		{name: "pid zero", pid: 0},
		{name: "negative pid", pid: -5},
		{name: "matching row", pid: pid, row: rowOf(pid), queries: 1, info: true},
		{name: "other pid", pid: pid, row: rowOf(pid + 1), queries: 1, err: syscall.EIO},
		{name: "no row", pid: pid, queries: 1, err: syscall.EIO},
		{name: "empty answer, probe ESRCH", pid: pid, rowErr: syscall.EIO, probeErr: syscall.ESRCH, queries: 1, probes: 1, absent: true},
		{name: "empty answer, probe nil", pid: pid, rowErr: syscall.EIO, queries: 1, probes: 1},
		{name: "empty answer, probe EPERM", pid: pid, rowErr: syscall.EIO, probeErr: syscall.EPERM, queries: 1, probes: 1},
		{name: "empty answer, probe EINTR", pid: pid, rowErr: syscall.EIO, probeErr: syscall.EINTR, queries: 1, probes: 1},
		{name: "query ESRCH, probe nil", pid: pid, rowErr: syscall.ESRCH, queries: 1, probes: 1},
		{name: "query ENOENT, probe EPERM", pid: pid, rowErr: syscall.ENOENT, probeErr: syscall.EPERM, queries: 1, probes: 1},
		{name: "query ESRCH, probe ESRCH", pid: pid, rowErr: syscall.ESRCH, probeErr: syscall.ESRCH, queries: 1, probes: 1, absent: true},
		{name: "query ENOMEM", pid: pid, rowErr: syscall.ENOMEM, queries: 1, err: syscall.ENOMEM},
		{name: "query EPERM", pid: pid, rowErr: syscall.EPERM, queries: 1, err: syscall.EPERM},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { checkReadCase(t, c) })
	}
}

func checkReadCase(t *testing.T, c readCase) {
	t.Helper()
	queries, probes := useReadSeams(t, c)
	info, err := Read(c.pid)
	checkReadAbsence(t, c, err)
	if *queries != c.queries || *probes != c.probes {
		t.Fatalf("%s: %d row queries and %d probes, want %d and %d", c.name, *queries, *probes, c.queries, c.probes)
	}
	switch {
	case c.info && (err != nil || info.PID != c.pid):
		t.Fatalf("a matching row was not returned: %+v %v", info, err)
	case !c.info && (err == nil || info.PID != 0):
		t.Fatalf("a row for another pid was returned as the process (%s): %+v %v", c.name, info, err)
	case c.err != nil && !errors.Is(err, c.err):
		t.Fatalf("%s: error %v, want %v", c.name, err, c.err)
	}
}

// checkReadAbsence: the callers' absence predicate holds only for a probe's
// ESRCH.
func checkReadAbsence(t *testing.T, c readCase, err error) {
	t.Helper()
	gone := processGone(err)
	switch {
	case c.absent && !gone:
		t.Fatalf("a proven absence was not reported absent (%s): %v", c.name, err)
	case c.absent || !gone:
		return
	case errors.Is(c.rowErr, syscall.ESRCH) || errors.Is(c.rowErr, syscall.ENOENT):
		t.Fatalf("an absence-looking query error skipped the probe (%s): %v", c.name, err)
	case c.probes == 1:
		t.Fatalf("an unproven probe was reported absent (%s): %v", c.name, err)
	default:
		t.Fatalf("%s was reported absent: %v", c.name, err)
	}
}

func useReadSeams(t *testing.T, c readCase) (queries, probes *int) {
	t.Helper()
	queries, probes = new(int), new(int)
	oldQuery, oldProbe := rowQuery, probe
	rowQuery = func(int) (*unix.KinfoProc, error) { *queries++; return c.row, c.rowErr }
	probe = func(int) error { *probes++; return c.probeErr }
	t.Cleanup(func() { rowQuery, probe = oldQuery, oldProbe })
	return queries, probes
}

// checkReadSelector: production Read uses fixedRow, and fixedRow is the
// one-row x/sys helper, never the slice helper with its size query and
// ENOMEM loop.
func checkReadSelector(t *testing.T) {
	if reflect.ValueOf(rowQuery).Pointer() != reflect.ValueOf(fixedRow).Pointer() {
		t.Fatal("Read used a query without a fixed row")
	}
	file, err := parser.ParseFile(token.NewFileSet(), "process_darwin.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "fixedRow" && fn.Name.Name != "Read" {
			continue
		}
		var calls []string
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "unix" {
					calls = append(calls, sel.Sel.Name)
				}
			}
			return true
		})
		want := []string{"SysctlKinfoProc"}
		if fn.Name.Name == "Read" {
			want = nil
		}
		if !reflect.DeepEqual(calls, want) {
			t.Fatalf("Read used a query without a fixed row: %s calls unix.%v", fn.Name.Name, calls)
		}
	}
}

// ownedChild has one Wait owner: wait joins once; the cleanup registered at
// Start kills (a no-op after the reap) and joins on every exit path,
// including a skip or a failure before the test's own Kill.
type ownedChild struct {
	cmd  *exec.Cmd
	once sync.Once
	err  error
}

func startOwned(t *testing.T, name string, args ...string) *ownedChild {
	t.Helper()
	c := &ownedChild{cmd: exec.CommandContext(t.Context(), name, args...)}
	if err := c.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.cmd.Process.Kill(); _ = c.wait() })
	return c
}

func (c *ownedChild) wait() error {
	c.once.Do(func() { c.err = c.cmd.Wait() })
	return c.err
}

// checkReadRealChildren uses owned children only, each with one Wait owner.
func checkReadRealChildren(t *testing.T) {
	t.Run("early stop is joined", checkEarlyStopJoined)
	running := startOwned(t, "/bin/sleep", "5")
	info, err := Read(running.cmd.Process.Pid)
	if errors.Is(err, syscall.EPERM) {
		t.Skipf("sandbox forbids process inspection: %v", err)
	}
	if err != nil || info.PID != running.cmd.Process.Pid || info.UID != uint32(os.Getuid()) || info.Start == "" || info.Zombie {
		t.Fatalf("a running child was not read: %+v %v", info, err)
	}
	_ = running.cmd.Process.Kill()
	_ = running.wait()
	if _, err := Read(running.cmd.Process.Pid); !processGone(err) {
		t.Fatalf("a proven absence was not reported absent (reaped child): %v", err)
	}
	zombie := startOwned(t, "/usr/bin/true") // unreaped until its cleanup
	until := time.Now().Add(2 * time.Second)
	for time.Now().Before(until) {
		if info, err := Read(zombie.cmd.Process.Pid); err == nil && info.Zombie {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("an unreaped exited child was not read as a zombie")
}

// checkEarlyStopJoined: an injected inspection error makes a subtest return
// early, before its own Kill; the cleanup registered at start must still join
// the child. A parent fallback (the same once-owner) runs after the assertion,
// so a child whose cleanup was not registered is still ended and joined.
func checkEarlyStopJoined(t *testing.T) {
	var child *ownedChild
	defer func() {
		if child != nil {
			_ = child.cmd.Process.Kill()
			_ = child.wait()
		}
	}()
	t.Run("returns early", func(t *testing.T) {
		child = startOwned(t, "/bin/sleep", "5")
		useReadSeams(t, readCase{rowErr: syscall.EPERM})
		if _, err := Read(child.cmd.Process.Pid); errors.Is(err, syscall.EPERM) {
			return // the early exit before the test's own Kill
		}
		t.Error("the injected inspection error was not returned")
	})
	if child == nil || child.cmd.ProcessState == nil {
		t.Fatal("a started child was not joined after an early stop")
	}
}
