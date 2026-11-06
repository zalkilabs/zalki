//go:build darwin || linux

package bus

import (
	"os/exec"
	"testing"
	"time"
)

func startChild(t *testing.T) (*exec.Cmd, ProcessRef) {
	t.Helper()
	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	p, err := readProcess(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return child, ProcessRef{PID: p.PID, Start: p.Start, Role: "client"}
}

// End to end with the real source: the sweep is set so long it cannot be what
// delivers the notices.
func TestObserverNoticesAKilledChildWithinASecondWithoutTheSweep(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	child, ref := startChild(t)
	watchedTarget(t, service, &ref)
	runObserver(t, service, neverSweeps, newExitSource)
	time.Sleep(300 * time.Millisecond)
	if counter.count() != 0 {
		t.Fatal("noticed while the process was running")
	}
	killed := time.Now()
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	waitFor(t, "ended notice", func() bool { return counter.count() == 1 })
	if took := time.Since(killed); took > time.Second {
		t.Fatalf("the notice took %s", took)
	}
	time.Sleep(200 * time.Millisecond)
	if counter.count() != 1 {
		t.Fatalf("delivered %d notices", counter.count())
	}
}

func TestObserverPicksUpAWatchArmedLaterThroughTheRealDirectoryEvent(t *testing.T) {
	service := newTestService(t)
	counter := &noticeCounter{}
	service.Deliver = counter.hook
	child, ref := startChild(t)
	stop := runObserver(t, service, neverSweeps, newExitSource)
	defer stop()
	time.Sleep(200 * time.Millisecond) // let the observer start and watch the empty directory
	watchedTarget(t, service, &ref)
	time.Sleep(300 * time.Millisecond) // directory event, settle, owner registration
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	waitFor(t, "ended notice for a watch armed after the observer started", func() bool { return counter.count() == 1 })
}

func TestTwoObserversWithRealSourcesDeliverOneNotice(t *testing.T) {
	first := newTestService(t)
	counter := &noticeCounter{}
	first.Deliver = counter.hook
	second := Service{Store: first.Store, Deliver: counter.hook}
	child, ref := startChild(t)
	watchedTarget(t, first, &ref)
	runObserver(t, first, neverSweeps, newExitSource)
	runObserver(t, second, neverSweeps, newExitSource)
	time.Sleep(300 * time.Millisecond)
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	waitFor(t, "ended notice", func() bool { return counter.count() >= 1 })
	time.Sleep(300 * time.Millisecond)
	if counter.count() != 1 {
		t.Fatalf("delivered %d notices", counter.count())
	}
}
