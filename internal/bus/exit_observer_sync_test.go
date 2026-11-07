package bus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/procwatch"
)

// These tests close gaps found in exitObserver.sync (exit_observer.go). They
// call sync directly, with a fake exit source, so each step is deterministic.
// Odd results are characterized, not endorsed.
//
// sync does this, in this order:
//  1. canWrite: a fenced observer, or a journal marker from a newer build,
//     ends the call. Nothing is watched, read or delivered.
//  2. If there is an exit source and the root was usable, watch the watches
//     directory. A failure is logged and ignored.
//  3. Lstat the root. If it is missing or is not a directory, reconcile with
//     NOTHING wanted (every owner process is unwatched) and return.
//  4. Read the root. If that fails, return WITHOUT reconciling: the owner
//     processes watched before stay watched.
//  5. For each entry: skip names that are not a canonical <kind>_<uuid>
//     directory; watch the directory BEFORE reading it (so a watch added in
//     between raises an event), even if it turns out to be empty; skip an
//     empty or unreadable directory; load the session (an unreadable one is
//     skipped); a retired session, or one whose owner exited, is queued for a
//     notice; otherwise its owner process (pid > 1) is wanted.
//  6. Reconcile: the source watches exactly the wanted owner processes.
//  7. For each queued target: stop if the context is done, else send its notice.
//     Step 6 happens before any notice is sent.

// newSyncObserver is an observer for one service, over the given source.
func newSyncObserver(service Service, source procwatch.Source) *exitObserver {
	return &exitObserver{
		service: &service, root: filepath.Join(service.Store.Dir, "watches"), source: source,
		procs: map[procwatch.Identity]struct{}{}, owners: map[procwatch.Identity][]string{},
	}
}

// refusingDirSource is a fake source that refuses to watch directories.
type refusingDirSource struct {
	*fakeExitSource
	attempts []string
}

func (r *refusingDirSource) WatchDir(path string) error {
	r.attempts = append(r.attempts, path)
	return errors.New("directory watch refused")
}

func TestSyncDoesNothingForAFencedObserver(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	exitedWatchedTarget(t, service)
	fake := newFakeExitSource()
	o := newSyncObserver(service, fake)
	o.fenced = true
	o.sync(context.Background())
	if len(fake.dirs) != 0 || len(fake.procs) != 0 || counter.count() != 0 {
		t.Fatalf("a fenced observer watched %v %v or delivered %d notices", fake.dirs, fake.procs, counter.count())
	}
}

func TestSyncWithAMissingRootUnwatchesEveryOwner(t *testing.T) {
	service := newTestService(t)
	owner := selfOwner(t)
	watchedTarget(t, service, owner)
	fake := newFakeExitSource()
	o := newSyncObserver(service, fake)
	o.sync(context.Background())
	if !fake.watchingProcess(*owner) || len(o.procs) != 1 || len(o.owners) != 1 {
		t.Fatalf("setup: the owner was not registered: %v %v", o.procs, o.owners)
	}
	for _, remove := range []struct {
		name  string
		setup func(root string) error
	}{
		{"the directory is missing", os.RemoveAll},
		{"the root is a file, not a directory", func(root string) error {
			if err := os.RemoveAll(root); err != nil {
				return err
			}
			return os.WriteFile(root, []byte("x"), 0600)
		}},
	} {
		t.Run(remove.name, func(t *testing.T) {
			fresh := newFakeExitSource()
			observer := newSyncObserver(service, fresh)
			observer.procs[procwatch.Identity{PID: owner.PID, Start: owner.Start}] = struct{}{}
			observer.owners[procwatch.Identity{PID: owner.PID, Start: owner.Start}] = []string{"cursor:" + newID()}
			if err := fresh.WatchProcess(procwatch.Identity{PID: owner.PID, Start: owner.Start}); err != nil {
				t.Fatal(err)
			}
			if err := remove.setup(observer.root); err != nil {
				t.Fatal(err)
			}
			observer.sync(context.Background())
			if fresh.watchingProcess(*owner) || len(observer.procs) != 0 || len(observer.owners) != 0 {
				t.Fatalf("owners stayed registered: source=%v procs=%v owners=%v", fresh.procs, observer.procs, observer.owners)
			}
			if !fresh.watchingDir(observer.root) {
				t.Fatal("the root directory was not watched before it was checked")
			}
		})
	}
}

// Characterized, not endorsed: a root that cannot be read keeps the old
// registrations, while a missing root clears them.
func TestSyncKeepsRegistrationsWhenTheRootCannotBeRead(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a directory without permission")
	}
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	owner := selfOwner(t)
	watchedTarget(t, service, owner)
	exitedWatchedTarget(t, service)
	fake := newFakeExitSource()
	o := newSyncObserver(service, fake)
	if err := os.Chmod(o.root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(o.root, 0700) })
	o.procs[procwatch.Identity{PID: owner.PID, Start: owner.Start}] = struct{}{}
	o.owners[procwatch.Identity{PID: owner.PID, Start: owner.Start}] = []string{"cursor:" + newID()}
	if err := fake.WatchProcess(procwatch.Identity{PID: owner.PID, Start: owner.Start}); err != nil {
		t.Fatal(err)
	}
	o.sync(context.Background())
	if !fake.watchingProcess(*owner) || len(o.procs) != 1 || len(o.owners) != 1 {
		t.Fatalf("registrations changed: %v %v", o.procs, o.owners)
	}
	if counter.count() != 0 {
		t.Fatal("notices were sent from a root that could not be read")
	}
}

func TestSyncIgnoresDirectoryWatchFailures(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	owner := selfOwner(t)
	_, _, live := watchedTarget(t, service, owner)
	exitedWatchedTarget(t, service)
	source := &refusingDirSource{fakeExitSource: newFakeExitSource()}
	o := newSyncObserver(service, source)
	o.sync(context.Background())
	// The root and every target directory were tried (the failure is only logged).
	if len(source.attempts) != 3 || source.attempts[0] != o.root {
		t.Fatalf("directory watches tried: %v", source.attempts)
	}
	if !source.watchingProcess(*owner) || len(o.owners) != 1 || o.owners[procwatch.Identity{PID: owner.PID, Start: owner.Start}][0] != live.ID {
		t.Fatalf("the live owner was not registered despite the failures: %v %v", source.procs, o.owners)
	}
	if counter.count() != 1 {
		t.Fatalf("the exited target was noticed %d times, want once", counter.count())
	}
}

func TestSyncSkipsTheRootWatchOnlyWhenTheRootIsUnusable(t *testing.T) {
	service := newTestService(t)
	watchedTarget(t, service, selfOwner(t))
	source := &refusingDirSource{fakeExitSource: newFakeExitSource()}
	o := newSyncObserver(service, source)
	o.rootUnusable = true
	o.sync(context.Background())
	for _, path := range source.attempts {
		if path == o.root {
			t.Fatal("the root was watched although it was marked unusable")
		}
	}
	if len(source.attempts) != 1 {
		t.Fatalf("target directories tried: %v, want the one with a watch", source.attempts)
	}
}

func TestSyncWatchesADirectoryBeforeReadingItEvenWhenItIsEmpty(t *testing.T) {
	service := newTestService(t)
	fake := newFakeExitSource()
	o := newSyncObserver(service, fake)
	if err := os.MkdirAll(o.root, 0700); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(o.root, "cursor_"+newID())
	notCanonical := filepath.Join(o.root, "not-canonical")
	for _, dir := range []string{empty, notCanonical} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	file := filepath.Join(o.root, "cursor_"+newID())
	link := filepath.Join(o.root, "cursor_"+newID())
	if err := os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	o.sync(context.Background())
	if !fake.watchingDir(empty) {
		t.Fatal("an empty target directory was not watched")
	}
	for _, skipped := range []string{notCanonical, file, link} {
		if fake.watchingDir(skipped) {
			t.Fatalf("%s was watched; only canonical real directories are", skipped)
		}
	}
}

func TestSyncRegistersOwnersBeforeAnyNoticeIsSent(t *testing.T) {
	service := newTestService(t)
	owner := selfOwner(t)
	watchedTarget(t, service, owner)
	exitedWatchedTarget(t, service)
	fake := newFakeExitSource()
	var registeredAtNotice []bool
	service.Deliver = func(_ context.Context, _ Message, _ Session) Receipt {
		registeredAtNotice = append(registeredAtNotice, fake.watchingProcess(*owner))
		return NewReceipt(StatusSubmitted)
	}
	o := newSyncObserver(service, fake)
	o.sync(context.Background())
	if len(registeredAtNotice) != 1 || !registeredAtNotice[0] {
		t.Fatalf("owner registered when the notice went out: %v, want one notice after registration", registeredAtNotice)
	}
}

func TestSyncStopsSendingNoticesOnceTheContextIsDone(t *testing.T) {
	service := newTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	delivered := 0
	service.Deliver = func(_ context.Context, _ Message, _ Session) Receipt {
		delivered++
		cancel() // the observer is stopped while it sends the first notice
		return NewReceipt(StatusSubmitted)
	}
	exitedWatchedTarget(t, service)
	exitedWatchedTarget(t, service)
	o := newSyncObserver(service, newFakeExitSource())
	done := make(chan struct{})
	go func() { defer close(done); o.sync(ctx) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("sync did not return after the context was cancelled")
	}
	if delivered != 1 {
		t.Fatalf("delivered %d notices, want the first only", delivered)
	}
	remaining := 0
	entries, err := os.ReadDir(o.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		files, err := os.ReadDir(filepath.Join(o.root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		remaining += len(files)
	}
	if remaining != 1 {
		t.Fatalf("%d watch records left, want the second target's one", remaining)
	}
}

func TestSyncSendsNoNoticeWhenTheContextIsAlreadyDone(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	exitedWatchedTarget(t, service)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o := newSyncObserver(service, newFakeExitSource())
	o.sync(ctx)
	if counter.count() != 0 {
		t.Fatalf("sent %d notices after the context was done", counter.count())
	}
}

// An owner with process ID 1 or lower is not a process the observer can watch
// (it is the init process, or no process at all): it is never registered, and
// its target is neither noticed nor treated as exited.
func TestSyncIgnoresOwnerPIDsOfOneOrLess(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	for _, pid := range []int{1, 0} {
		watchedTarget(t, service, &ProcessRef{PID: pid, Start: "1", Role: "client"})
	}
	fake := newFakeExitSource()
	o := newSyncObserver(service, fake)
	o.sync(context.Background())
	if len(fake.procs) != 0 || len(o.procs) != 0 || len(o.owners) != 0 {
		t.Fatalf("an owner that is not a real process was registered: %v %v %v", fake.procs, o.procs, o.owners)
	}
	if counter.count() != 0 {
		t.Fatalf("%d notices were sent for owners that cannot be probed", counter.count())
	}
}
