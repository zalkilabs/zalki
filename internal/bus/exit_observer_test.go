package bus

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Short enough that a test waiting for a sweep is quick, long enough that
// event-driven tests cannot pass by sweeping.
const (
	testSweep   = 30 * time.Millisecond
	neverSweeps = time.Hour
)

// noticeCounter is a Deliver hook that records ended notices across services.
type noticeCounter struct {
	mu     sync.Mutex
	bodies []string
}

func (c *noticeCounter) hook(_ context.Context, msg Message, _ Session) Receipt {
	c.mu.Lock()
	c.bodies = append(c.bodies, msg.Body)
	c.mu.Unlock()
	return NewReceipt(StatusSubmitted)
}

func (c *noticeCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func (c *noticeCounter) first() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.bodies) == 0 {
		return ""
	}
	return c.bodies[0]
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// fakeExitSource is a controllable exitSource for the portable observer logic.
type fakeExitSource struct {
	mu        sync.Mutex
	events    chan exitEvent
	procs     map[processKey]bool
	dirs      map[string]bool
	watchFail error
	closed    bool
}

func newFakeExitSource() *fakeExitSource {
	return &fakeExitSource{events: make(chan exitEvent, 16), procs: map[processKey]bool{}, dirs: map[string]bool{}}
}

func (f *fakeExitSource) WatchProcess(ref ProcessRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.watchFail != nil {
		return f.watchFail
	}
	f.procs[keyOf(ref)] = true
	return nil
}
func (f *fakeExitSource) Unwatch(ref ProcessRef) {
	f.mu.Lock()
	delete(f.procs, keyOf(ref))
	f.mu.Unlock()
}
func (f *fakeExitSource) WatchDir(path string) error {
	f.mu.Lock()
	f.dirs[path] = true
	f.mu.Unlock()
	return nil
}
func (f *fakeExitSource) Events() <-chan exitEvent { return f.events }
func (f *fakeExitSource) Close() error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}
func (f *fakeExitSource) watchingProcess(ref ProcessRef) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.procs[keyOf(ref)]
}
func (f *fakeExitSource) watchingDir(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dirs[path]
}
func (f *fakeExitSource) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *fakeExitSource) open() (exitSource, error) { return f, nil }

func unsupportedExitSource() (exitSource, error) { return nil, errExitSourceUnsupported }

var (
	deadOwner = ProcessRef{PID: 1 << 30, Start: "1", Role: "client"}
	selfOwner = func(t *testing.T) *ProcessRef {
		t.Helper()
		p, err := readProcess(os.Getpid())
		if err != nil {
			t.Fatal(err)
		}
		return &ProcessRef{PID: p.PID, Start: p.Start, Role: "client"}
	}
)

func setOwner(t *testing.T, store Store, id string, owner *ProcessRef) {
	t.Helper()
	current, err := store.session(id)
	if err != nil {
		t.Fatal(err)
	}
	current.Owner = owner
	if err := store.saveSession(current); err != nil {
		t.Fatal(err)
	}
}

// watchedTarget arms a watch on a new cursor target owned by owner.
func watchedTarget(t *testing.T, service Service, owner *ProcessRef) (watcher Session, key string, target Session) {
	t.Helper()
	watcher, key, target, _ = registerPair(t, service.Store, "codex", "cursor")
	setOwner(t, service.Store, target.ID, owner)
	if result, err := service.Watch(watcher.ID, key, target.ID); err != nil || result.Status != "watching" {
		t.Fatal(result, err)
	}
	return watcher, key, target
}

// exitedWatchedTarget arms a watch on a target and then gives it a dead owner, as
// after a kill: watch_agent answers an already-exited target immediately, so a
// watch can only exist for one that was alive when it was armed.
func exitedWatchedTarget(t *testing.T, service Service) (watcher Session, key string, target Session) {
	t.Helper()
	watcher, key, target = watchedTarget(t, service, selfOwner(t))
	setOwner(t, service.Store, target.ID, &deadOwner)
	return watcher, key, target
}

// runObserver runs the observer until the test ends or the returned stop is called.
func runObserver(t *testing.T, service Service, sweep time.Duration, open func() (exitSource, error)) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.observeExits(ctx, sweep, open)
	}()
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Error("observer did not stop after cancellation")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestObserverNoticesAProcessExitEventWithoutAnySweep(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	// The owner is alive by session record until the test changes it.
	setLive := selfOwner(t)
	watcher, _, target := watchedTarget(t, service, setLive)
	fake := newFakeExitSource()
	runObserver(t, service, neverSweeps, fake.open)
	waitFor(t, "owner registration", func() bool { return fake.watchingProcess(*setLive) })
	if !fake.watchingDir(filepath.Join(service.Store.Dir, "watches")) {
		t.Fatal("the watches directory was not watched")
	}
	if counter.count() != 0 {
		t.Fatal("noticed a live target")
	}
	// The process exits: the record now shows a dead owner and the kernel says so.
	setOwner(t, service.Store, target.ID, &deadOwner)
	fake.events <- exitEvent{Kind: exitProcess, Process: *setLive}
	waitFor(t, "ended notice", func() bool { return counter.count() == 1 })
	if body := counter.first(); body == "" || !strings.Contains(body, ") ended the session at ") {
		t.Fatalf("notice body: %q", body)
	}
	if _, err := os.Stat(mustWatchPath(t, service.Store, watcher.ID, target.ID)); !os.IsNotExist(err) {
		t.Fatal("watch was not consumed", err)
	}
}

func TestObserverPicksUpAWatchArmedAfterItStartedFromADirectoryEvent(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	fake := newFakeExitSource()
	runObserver(t, service, neverSweeps, fake.open)
	waitFor(t, "root watch", func() bool { return fake.watchingDir(filepath.Join(service.Store.Dir, "watches")) })

	owner := selfOwner(t)
	_, _, target := watchedTarget(t, service, owner)
	if fake.watchingProcess(*owner) {
		t.Fatal("registered an owner before any directory event")
	}
	fake.events <- exitEvent{Kind: exitDirChanged}
	waitFor(t, "owner registration after a directory event", func() bool { return fake.watchingProcess(*owner) })
	if !fake.watchingDir(filepath.Join(service.Store.Dir, "watches", strings.ReplaceAll(target.ID, ":", "_"))) {
		t.Fatal("the new target directory was not watched")
	}
}

func TestTwoObserversDeliverExactlyOneNotice(t *testing.T) {
	first := newTestService(t)
	counter := &noticeCounter{}
	first.Deliver = counter.hook
	second := Service{Store: first.Store, Deliver: counter.hook}
	owner := selfOwner(t)
	_, _, target := watchedTarget(t, first, owner)
	fakeA, fakeB := newFakeExitSource(), newFakeExitSource()
	runObserver(t, first, neverSweeps, fakeA.open)
	runObserver(t, second, neverSweeps, fakeB.open)
	waitFor(t, "both observers watching", func() bool { return fakeA.watchingProcess(*owner) && fakeB.watchingProcess(*owner) })
	setOwner(t, first.Store, target.ID, &deadOwner)
	fakeA.events <- exitEvent{Kind: exitProcess, Process: *owner}
	fakeB.events <- exitEvent{Kind: exitProcess, Process: *owner}
	waitFor(t, "ended notice", func() bool { return counter.count() >= 1 })
	time.Sleep(200 * time.Millisecond)
	if counter.count() != 1 {
		t.Fatalf("delivered %d notices", counter.count())
	}
}

func TestObserverIgnoresAnExitEventForARecycledOwnerPID(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	owner := selfOwner(t)
	watcher, _, target := watchedTarget(t, service, owner)
	fake := newFakeExitSource()
	runObserver(t, service, neverSweeps, fake.open)
	waitFor(t, "owner registration", func() bool { return fake.watchingProcess(*owner) })
	// The session re-registered under a new live owner; a late event for the old
	// process must not produce an "ended" notice.
	setOwner(t, service.Store, target.ID, selfOwner(t))
	fake.events <- exitEvent{Kind: exitProcess, Process: *owner}
	time.Sleep(200 * time.Millisecond)
	if counter.count() != 0 {
		t.Fatal("noticed a session that is still alive")
	}
	if _, err := os.Stat(mustWatchPath(t, service.Store, watcher.ID, target.ID)); err != nil {
		t.Fatal("the watch was consumed", err)
	}
}

func TestObserverFallsBackToTheSweepWithoutAnExitSource(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	_, _, target := watchedTarget(t, service, selfOwner(t))
	runObserver(t, service, testSweep, unsupportedExitSource)
	time.Sleep(4 * testSweep)
	if counter.count() != 0 {
		t.Fatal("noticed a live target")
	}
	setOwner(t, service.Store, target.ID, &deadOwner)
	waitFor(t, "sweep notice", func() bool { return counter.count() == 1 })
}

func TestObserverKeepsSweepingAfterTheExitSourceFails(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	owner := selfOwner(t)
	_, _, target := watchedTarget(t, service, owner)
	fake := newFakeExitSource()
	runObserver(t, service, testSweep, fake.open)
	waitFor(t, "owner registration", func() bool { return fake.watchingProcess(*owner) })
	fake.events <- exitEvent{Kind: exitSourceFailed}
	setOwner(t, service.Store, target.ID, &deadOwner)
	waitFor(t, "sweep notice after source failure", func() bool { return counter.count() == 1 })
}

func TestObserverSweepsWhenOwnerRegistrationFails(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	_, _, target := watchedTarget(t, service, selfOwner(t))
	fake := newFakeExitSource()
	fake.watchFail = os.ErrPermission
	runObserver(t, service, testSweep, fake.open)
	time.Sleep(4 * testSweep)
	setOwner(t, service.Store, target.ID, &deadOwner)
	waitFor(t, "sweep notice", func() bool { return counter.count() == 1 })
}

// holdJournalLock holds the journal lock until the test ends, so a test can show
// that the observer's scan never waits for it.
func holdJournalLock(t *testing.T, store Store) {
	t.Helper()
	released, held := make(chan struct{}), make(chan struct{})
	go func() {
		_ = store.locked(func() error {
			close(held)
			<-released
			return nil
		})
	}()
	<-held
	t.Cleanup(func() { close(released) })
}

func TestObserverIsIdleWithoutWatches(t *testing.T) {
	service := newTestService(t)
	service.Deliver = (&noticeCounter{}).hook
	holdJournalLock(t, service.Store)
	fake := newFakeExitSource()
	runObserver(t, service, neverSweeps, fake.open)
	root := filepath.Join(service.Store.Dir, "watches")
	waitFor(t, "the idle observer to watch the directory", func() bool { return fake.watchingDir(root) })
	time.Sleep(100 * time.Millisecond)
	fake.mu.Lock()
	registered := len(fake.procs)
	fake.mu.Unlock()
	if registered != 0 {
		t.Fatalf("registered %d processes with no watches", registered)
	}
	if _, err := os.Lstat(filepath.Join(service.Store.Dir, observerStampName)); !os.IsNotExist(err) {
		t.Fatal("the observer wrote the sweep stamp without sweeping", err)
	}
}

func TestObserverScanDoesNotNeedTheJournalLock(t *testing.T) {
	service := newTestService(t)
	service.Deliver = (&noticeCounter{}).hook
	owner := selfOwner(t)
	watchedTarget(t, service, owner)
	holdJournalLock(t, service.Store)
	fake := newFakeExitSource()
	runObserver(t, service, neverSweeps, fake.open)
	waitFor(t, "owner registration while the journal lock is held", func() bool { return fake.watchingProcess(*owner) })
}

func TestObserverClaimsASweepOncePerHalfIntervalAcrossServers(t *testing.T) {
	store := fixtureStore(t)
	if err := privateDir(store.Dir); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	interval := 20 * time.Second
	if !store.claimObservation(interval, now) {
		t.Fatal("the first server did not sweep")
	}
	if store.claimObservation(interval, now.Add(interval/4)) {
		t.Fatal("a second server swept within half an interval")
	}
	if !store.claimObservation(interval, now.Add(interval)) {
		t.Fatal("no server swept after the interval")
	}
	stamp := filepath.Join(store.Dir, observerStampName)
	info, err := os.Stat(stamp)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("stamp: %v %v", info, err)
	}
	// An unusable stamp means sweep anyway, and a symlink is never followed.
	if err := os.Remove(stamp); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	past := now.Add(-time.Hour)
	if err := os.Chtimes(victim, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, stamp); err != nil {
		t.Fatal(err)
	}
	for range 2 { // repeated: a stamp that cannot be used must never suppress a sweep
		if !store.claimObservation(interval, now) {
			t.Fatal("a symlinked stamp suppressed the sweep")
		}
	}
	if got, _ := os.Stat(victim); !got.ModTime().Equal(past) {
		t.Fatal("followed the stamp symlink")
	}
}

func TestObserverRechecksExitUnderTheLock(t *testing.T) {
	service := newTestService(t)
	service.Deliver = (&noticeCounter{}).hook
	watcher, _, target := exitedWatchedTarget(t, service)
	// The session re-registered with a live owner between the scan and the lock.
	setOwner(t, service.Store, target.ID, selfOwner(t))
	if _, watches, err := service.consumeIfExited(target.ID); err != nil || len(watches) != 0 {
		t.Fatalf("consumed for a live session: %v %v", watches, err)
	}
	if _, err := os.Stat(mustWatchPath(t, service.Store, watcher.ID, target.ID)); err != nil {
		t.Fatal("watch removed for a live session", err)
	}
	// The same PID with a different start time is a reused PID, not the owner.
	reused := selfOwner(t)
	reused.Start = "an-earlier-process"
	setOwner(t, service.Store, target.ID, reused)
	if _, watches, err := service.consumeIfExited(target.ID); err != nil || len(watches) != 1 {
		t.Fatalf("a reused pid hid the exit: %v %v", watches, err)
	}
}

func TestObserverStartupNoticesRetiredTargetsAndSkipsExpiredWatches(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	watcher, key, retired, _ := registerPair(t, service.Store, "codex", "cursor")
	expired, _ := register(t, service.Store, "cursor")
	if _, err := service.Watch(watcher.ID, key, retired.ID); err != nil {
		t.Fatal(err)
	}
	writeTestWatch(t, service.Store, watcher.ID, expired.ID, time.Now().Add(-time.Minute))
	for _, id := range []string{retired.ID, expired.ID} {
		current, err := service.Store.session(id)
		if err != nil {
			t.Fatal(err)
		}
		current.Retired, current.State = true, StateRetired
		if err := service.Store.saveSession(current); err != nil {
			t.Fatal(err)
		}
	}
	runObserver(t, service, neverSweeps, unsupportedExitSource)
	waitFor(t, "ended notice", func() bool { return counter.count() == 1 })
	waitFor(t, "expired watch removed silently", func() bool {
		_, err := os.Stat(mustWatchPath(t, service.Store, watcher.ID, expired.ID))
		return os.IsNotExist(err)
	})
	time.Sleep(100 * time.Millisecond)
	if counter.count() != 1 {
		t.Fatalf("delivered %d notices", counter.count())
	}
}

func TestObserverSkipsUntrustedEntriesAndStillNoticesOthers(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	watcher, _, _ := exitedWatchedTarget(t, service)
	root := filepath.Join(service.Store.Dir, "watches")

	corrupt, _ := register(t, service.Store, "cursor")
	writeTestWatch(t, service.Store, watcher.ID, corrupt.ID, time.Now().Add(time.Hour))
	if err := os.WriteFile(service.Store.sessionPathMust(t, corrupt.ID), []byte("not json"), 0600); err != nil {
		t.Fatal(err)
	}
	future, _ := register(t, service.Store, "cursor")
	writeTestWatch(t, service.Store, watcher.ID, future.ID, time.Now().Add(time.Hour))
	current, err := service.Store.session(future.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Schema = 1 << 20
	if err := writeJSON(service.Store.sessionPathMust(t, future.ID), current); err != nil {
		t.Fatal(err)
	}
	unknownAdapter := filepath.Join(root, "removed-client_"+newID())
	if err := os.MkdirAll(unknownAdapter, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unknownAdapter, "codex_"+newID()+".json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "cursor_"+newID())); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "not-canonical"), 0700); err != nil {
		t.Fatal(err)
	}
	fake := newFakeExitSource()
	runObserver(t, service, neverSweeps, fake.open)
	waitFor(t, "the trustworthy target's notice", func() bool { return counter.count() == 1 })
	time.Sleep(100 * time.Millisecond)
	if counter.count() != 1 {
		t.Fatalf("delivered %d notices", counter.count())
	}
}

func TestObserverStopsPromptlyWhenCancelledMidDelivery(t *testing.T) {
	service := newTestService(t)
	entered := make(chan struct{})
	var once sync.Once
	service.Deliver = func(ctx context.Context, _ Message, _ Session) Receipt {
		once.Do(func() { close(entered) })
		<-ctx.Done()
		return NewReceipt(StatusSubmitted)
	}
	exitedWatchedTarget(t, service)
	fake := newFakeExitSource()
	stop := runObserver(t, service, neverSweeps, fake.open)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the observer never reached delivery")
	}
	stop() // fails the test itself if the observer ignores cancellation
	if !fake.isClosed() {
		t.Fatal("the exit source was not closed")
	}
}

func TestObserverPanicStopsOnlyTheObserver(t *testing.T) {
	service := newTestService(t)
	attempted := make(chan struct{})
	var once sync.Once
	service.Deliver = func(context.Context, Message, Session) Receipt {
		once.Do(func() { close(attempted) })
		panic("transport bug")
	}
	exitedWatchedTarget(t, service)
	fake := newFakeExitSource()
	stop := runObserver(t, service, neverSweeps, fake.open)
	select {
	case <-attempted:
	case <-time.After(5 * time.Second):
		t.Fatal("no delivery attempt")
	}
	stop() // Reaching here means the panic did not escape the goroutine.
	waitFor(t, "exit source closed after the panic", fake.isClosed)
}
