package bus

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/procwatch"
)

// These tests close the gap found in Store.rememberOwner (process.go): no test
// reached the success path, where the owner process is found by walking up from
// os.Getppid(). That path is tested in a CHILD copy of the test binary (see the
// end of this file): the child's real parent is the test binary, a stable
// process the test owns, and the child runs the assertions itself. The other
// tests here do not depend on the parent process. Odd results are characterized,
// not endorsed.
//
// rememberOwner does this, in this order, under the journal lock:
//  1. Load the session. A missing session is not an error: nothing is done.
//  2. If the session is not retired, or this is a refresh:
//     a. an adapter that does not track its own process (a shared daemon): on
//     refresh, a recorded owner is dropped; otherwise nothing;
//     b. otherwise, if there is no owner, or this is a refresh: look for the
//     native owner process; if found, record it (even when it is the owner
//     already recorded); if not found, on refresh, drop a recorded owner that
//     has exited and keep one that is alive.
//  3. Always, even for a retired session without refresh: record the hook build
//     if it differs from the running one.
//  4. Save only if something changed.

func staleBuild() string { return "stale-build-" + currentExecutable.loadedBuild }

// ownerAdapter is a cursor-like adapter that names the given processes as its
// owner.
type ownerAdapter struct {
	stubCursorAdapter
	names []string
}

func (a ownerAdapter) Descriptor() AdapterDescriptor {
	descriptor := a.stubCursorAdapter.Descriptor()
	descriptor.ProcessNames = a.names
	return descriptor
}

// sharedAdapter is an ownerAdapter whose process is not the session.
type sharedAdapter struct{ ownerAdapter }

func (sharedAdapter) Liveness(LivenessView, LivenessProbes) Liveness { return LivenessUnknown }
func (sharedAdapter) TracksOwnProcess() bool                         { return false }

func ownerStore(t *testing.T, adapter Adapter) Store {
	t.Helper()
	registry, err := NewRegistry(stubCodexAdapter{}, stubClaudeAdapter{}, adapter)
	if err != nil {
		t.Fatal(err)
	}
	return Store{Dir: filepath.Join(t.TempDir(), "state"), Registry: registry}
}

// ownedSession registers a cursor session and gives it the given record state.
func ownedSession(t *testing.T, store Store, change func(v *Session)) Session {
	t.Helper()
	v, _, err := store.Register("cursor", newID(), "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	change(&v)
	if err := store.locked(func() error { return store.saveSession(v) }); err != nil {
		t.Fatal(err)
	}
	return v
}

func remembered(t *testing.T, store Store, v Session, refresh bool) Session {
	t.Helper()
	if err := store.rememberOwner(v.ID, "cursor", refresh); err != nil {
		t.Fatal(err)
	}
	current, err := store.session(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	return current
}

func sameOwner(a, b *ProcessRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestRememberOwnerOnARetiredSessionWithoutRefresh(t *testing.T) {
	// Two faults at once: it is not refreshed, and its stale hook build is still updated.
	store := ownerStore(t, ownerAdapter{names: []string{"no-such-process-name"}})
	v := ownedSession(t, store, func(v *Session) { v.Retired, v.State, v.HookBuild = true, StateRetired, staleBuild() })
	current := remembered(t, store, v, false)
	if current.Owner != nil || current.HookBuild != currentExecutable.loadedBuild {
		t.Fatalf("owner = %+v, build = %q; want no owner and the running build", current.Owner, current.HookBuild)
	}
}

func TestRememberOwnerForAnAdapterThatSharesItsProcess(t *testing.T) {
	store := ownerStore(t, sharedAdapter{ownerAdapter{names: []string{"no-such-process-name"}}})
	// A live process, so that only the adapter's policy can explain a drop.
	recorded := *selfOwner(t)
	build := func(v *Session) { v.HookBuild = currentExecutable.loadedBuild }
	t.Run("a recorded owner is kept without refresh", func(t *testing.T) {
		v := ownedSession(t, store, func(v *Session) { build(v); v.Owner = &recorded })
		if current := remembered(t, store, v, false); !sameOwner(current.Owner, &recorded) {
			t.Fatalf("owner = %+v, want it kept", current.Owner)
		}
	})
	t.Run("a recorded owner is dropped on refresh", func(t *testing.T) {
		v := ownedSession(t, store, func(v *Session) { build(v); v.Owner = &recorded })
		if current := remembered(t, store, v, true); current.Owner != nil {
			t.Fatalf("owner = %+v, want it dropped", current.Owner)
		}
	})
	t.Run("no owner is recorded on refresh", func(t *testing.T) {
		v := ownedSession(t, store, build)
		if current := remembered(t, store, v, true); current.Owner != nil {
			t.Fatalf("owner = %+v, want none for a shared process", current.Owner)
		}
	})
}

func TestRememberOwnerWithoutANativeOwner(t *testing.T) {
	store := ownerStore(t, ownerAdapter{names: []string{"no-such-process-name"}})
	build := func(v *Session) { v.HookBuild = currentExecutable.loadedBuild }
	alive := selfOwner(t)
	tests := []struct {
		name      string
		owner     *ProcessRef
		refresh   bool
		wantOwner *ProcessRef
	}{
		{"no owner and no refresh stays without one", nil, false, nil},
		{"no owner and refresh stays without one", nil, true, nil},
		{"an owner that is alive is kept on refresh", alive, true, alive},
		{"an owner that has exited is dropped on refresh", &deadOwner, true, nil},
		{"an owner that has exited is kept without refresh", &deadOwner, false, &deadOwner},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			v := ownedSession(t, store, func(v *Session) { build(v); v.Owner = test.owner })
			if current := remembered(t, store, v, test.refresh); !sameOwner(current.Owner, test.wantOwner) {
				t.Fatalf("owner = %+v, want %+v", current.Owner, test.wantOwner)
			}
		})
	}
}

func TestRememberOwnerSavesOnlyWhenSomethingChanged(t *testing.T) {
	store := ownerStore(t, ownerAdapter{names: []string{"no-such-process-name"}})
	t.Run("nothing to change: the record is not written", func(t *testing.T) {
		v := ownedSession(t, store, func(v *Session) { v.HookBuild = currentExecutable.loadedBuild })
		stat := func() time.Time {
			info, err := os.Stat(mustSessionPath(t, store, v.ID))
			if err != nil {
				t.Fatal(err)
			}
			return info.ModTime()
		}
		path := mustSessionPath(t, store, v.ID)
		old := time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		before := stat()
		remembered(t, store, v, false)
		if !stat().Equal(before) {
			t.Fatal("an unchanged session was written")
		}
	})
	t.Run("a stale hook build alone is saved", func(t *testing.T) {
		v := ownedSession(t, store, func(v *Session) { v.HookBuild = staleBuild() })
		if current := remembered(t, store, v, false); current.HookBuild != currentExecutable.loadedBuild {
			t.Fatalf("hook build = %q", current.HookBuild)
		}
	})
}

func TestRememberOwnerForAMissingSessionDoesNothing(t *testing.T) {
	store := ownerStore(t, ownerAdapter{names: []string{"no-such-process-name"}})
	id := "cursor:" + newID()
	if err := store.rememberOwner(id, "cursor", true); err != nil {
		t.Fatalf("a missing session was an error: %v", err)
	}
	if _, err := os.Stat(mustSessionPath(t, store, id)); !os.IsNotExist(err) {
		t.Fatal("a session record appeared", err)
	}
}

// The native owner is found by walking up from os.Getppid(). To make that
// parent stable and known, the test re-runs this test binary as a child: the
// child's parent is the test binary itself. The child checks that, runs the
// assertions, and prints a line with a one-time nonce only after all of them
// passed. The parent requires that line and a zero exit status, so a child that
// did not run, or was skipped, cannot count. The one documented skip is a
// platform or sandbox that forbids process inspection (EPERM or EACCES from
// reading the parent), which the child reports with its own marker line.

const (
	childEnvNonce = "ZALKI_TEST_REMEMBER_OWNER_NONCE"
	childEnvPID   = "ZALKI_TEST_REMEMBER_OWNER_PARENT_PID"
	childEnvStart = "ZALKI_TEST_REMEMBER_OWNER_PARENT_START"
	childSkipLine = "zalki-test-child-skip: process inspection is not permitted: "
	childOKLine   = "zalki-test-child-ok: "
)

func inspectionForbidden(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.EACCES)
}

// TestRememberOwnerChild is the child's body. Without the marker it does nothing.
func TestRememberOwnerChild(t *testing.T) {
	nonce := os.Getenv(childEnvNonce)
	if nonce == "" {
		return
	}
	parent, err := procwatch.Read(os.Getppid())
	if inspectionForbidden(err) {
		fmt.Println(childSkipLine + err.Error())
		return
	}
	if err != nil {
		t.Fatalf("the child cannot identify its parent: %v", err)
	}
	wantPID, _ := strconv.Atoi(os.Getenv(childEnvPID))
	if parent.PID != wantPID || parent.Start != os.Getenv(childEnvStart) {
		t.Fatalf("the child's parent is %d (start %q), want the test binary %d (start %q)", parent.PID, parent.Start, wantPID, os.Getenv(childEnvStart))
	}
	if parent.UID != uint32(os.Getuid()) {
		t.Fatalf("the parent belongs to user %d, not %d", parent.UID, os.Getuid())
	}
	want := ProcessRef{PID: parent.PID, Start: parent.Start, Role: "client"}
	checkNativeOwnerPath(t, parent.Name, want)
	if t.Failed() { // a failed subtest must not be reported as success
		return
	}
	fmt.Println(childOKLine + nonce)
}

// checkNativeOwnerPath runs the scenarios that need the real parent process.
func checkNativeOwnerPath(t *testing.T, parentName string, want ProcessRef) {
	t.Helper()
	store := ownerStore(t, ownerAdapter{names: []string{parentName}})
	t.Run("a session without an owner records its parent", func(t *testing.T) { childRecordsParent(t, store, want) })
	t.Run("an existing owner is kept without refresh and replaced on refresh", func(t *testing.T) { childReplacesOnRefresh(t, store, want) })
	t.Run("a retired session records its parent only on refresh", func(t *testing.T) { childRetiredRecordsOnRefresh(t, store, want) })
	t.Run("a new owner alone is saved", func(t *testing.T) {
		v := ownedSession(t, store, func(v *Session) { v.HookBuild = currentExecutable.loadedBuild })
		if current := remembered(t, store, v, false); !sameOwner(current.Owner, &want) {
			t.Fatalf("owner = %+v, want the parent", current.Owner)
		}
	})
	t.Run("an adapter that shares its process never records the parent", func(t *testing.T) {
		shared := ownerStore(t, sharedAdapter{ownerAdapter{names: []string{parentName}}})
		v := ownedSession(t, shared, func(v *Session) { v.HookBuild = currentExecutable.loadedBuild })
		if current := remembered(t, shared, v, true); current.Owner != nil {
			t.Fatalf("owner = %+v, want none for a shared process", current.Owner)
		}
	})
}

func childRecordsParent(t *testing.T, store Store, want ProcessRef) {
	t.Helper()
	v := ownedSession(t, store, func(v *Session) { v.HookBuild = staleBuild() })
	current := remembered(t, store, v, false)
	if !sameOwner(current.Owner, &want) {
		t.Fatalf("owner = %+v, want the parent process %+v", current.Owner, want)
	}
	if current.HookBuild != currentExecutable.loadedBuild {
		t.Fatalf("hook build = %q, want the running build", current.HookBuild)
	}
}

func childReplacesOnRefresh(t *testing.T, store Store, want ProcessRef) {
	t.Helper()
	previous := ProcessRef{PID: 4242, Start: "an earlier start", Role: "client"}
	for _, refresh := range []bool{false, true} {
		v := ownedSession(t, store, func(v *Session) { v.Owner = &previous; v.HookBuild = currentExecutable.loadedBuild })
		expected := &previous
		if refresh {
			expected = &want
		}
		if current := remembered(t, store, v, refresh); !sameOwner(current.Owner, expected) {
			t.Fatalf("refresh=%v: owner = %+v, want %+v", refresh, current.Owner, expected)
		}
	}
}

func childRetiredRecordsOnRefresh(t *testing.T, store Store, want ProcessRef) {
	t.Helper()
	retire := func(v *Session) { v.Retired, v.State, v.HookBuild = true, StateRetired, staleBuild() }
	v := ownedSession(t, store, retire)
	if current := remembered(t, store, v, false); current.Owner != nil {
		t.Fatalf("without refresh: owner = %+v, want none", current.Owner)
	}
	v = ownedSession(t, store, retire)
	if current := remembered(t, store, v, true); !sameOwner(current.Owner, &want) || !current.Retired {
		t.Fatalf("with refresh: owner = %+v retired = %v, want the parent and still retired", current.Owner, current.Retired)
	}
}

// TestRememberOwnerFindsItsParentInAControlledChild runs the child above and
// requires its success line.
func TestRememberOwnerFindsItsParentInAControlledChild(t *testing.T) {
	self, err := procwatch.Read(os.Getpid())
	if inspectionForbidden(err) {
		t.Skipf("process inspection is not permitted here: %v", err)
	}
	if err != nil {
		t.Fatalf("this test cannot identify itself: %v", err)
	}
	nonce := newID()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRememberOwnerChild$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		childEnvNonce+"="+nonce, childEnvPID+"="+strconv.Itoa(self.PID), childEnvStart+"="+self.Start)
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.WaitDelay = 5 * time.Second
	err = cmd.Run() // CommandContext kills the child when the deadline passes.
	text := output.String()
	switch {
	case ctx.Err() != nil:
		t.Fatalf("the child did not finish in time:\n%s", text)
	case err != nil:
		t.Fatalf("the child failed: %v\n%s", err, text)
	case strings.Contains(text, childSkipLine):
		t.Skipf("process inspection is not permitted for the child: %s", text)
	case !strings.Contains(text, childOKLine+nonce+"\n"):
		t.Fatalf("the child exited without reporting success (did it run?):\n%s", text)
	}
}
