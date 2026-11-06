//go:build darwin

package procwatch

import (
	"errors"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// kqueueSource implements Source with kqueue: EVFILT_PROC/NOTE_EXIT for
// process exit (it works for any process of the same user, not only children)
// and EVFILT_VNODE for directory changes.
type kqueueSource struct {
	kq     int
	mu     sync.Mutex
	procs  map[int]Identity // watched pid -> ref
	dirs   map[string]int   // path -> open descriptor
	byFD   map[int]string
	events chan Event
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

const kqueueWake = 0 // identifier of the EVFILT_USER event that stops the loop

// NewSource opens the platform process and directory notification source.
func NewSource() (Source, error) {
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(kq)
	wake := unix.Kevent_t{Ident: kqueueWake, Filter: unix.EVFILT_USER, Flags: unix.EV_ADD | unix.EV_CLEAR}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{wake}, nil, nil); err != nil {
		_ = unix.Close(kq)
		return nil, err
	}
	s := &kqueueSource{
		kq: kq, procs: map[int]Identity{}, dirs: map[string]int{}, byFD: map[int]string{},
		events: make(chan Event, 64), done: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.loop()
	return s, nil
}

// Events returns the raw notifications produced by this source.
func (s *kqueueSource) Events() <-chan Event { return s.events }

// emit never blocks the caller; the event is dropped once the source is closed.
func (s *kqueueSource) emit(ev Event) {
	go s.send(ev)
}

func (s *kqueueSource) send(ev Event) {
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

// register applies one change with a receipt, so a failure comes back as an
// error code rather than being lost in the event stream.
func (s *kqueueSource) register(change unix.Kevent_t) error {
	change.Flags |= unix.EV_RECEIPT
	receipt := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(s.kq, []unix.Kevent_t{change}, receipt, &unix.Timespec{})
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		for _, r := range receipt[:n] {
			if r.Flags&unix.EV_ERROR != 0 && r.Data != 0 {
				return syscall.Errno(r.Data)
			}
		}
		return nil
	}
}

// WatchProcess watches one process incarnation without waiting for a consumer.
func (s *kqueueSource) WatchProcess(ref Identity) error {
	if ref.PID <= 1 {
		return errors.New("invalid process id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if known, ok := s.procs[ref.PID]; ok {
		if known.Start != ref.Start {
			s.emit(Event{Kind: ProcessExited, Process: ref}) // the pid now belongs to another process
		}
		return nil
	}
	add := unix.Kevent_t{Ident: uint64(ref.PID), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	if err := s.register(add); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			s.emit(Event{Kind: ProcessExited, Process: ref})
			return nil
		}
		return err
	}
	// Verify after registering: the registration pins the incarnation we check.
	current, err := Read(ref.PID)
	switch {
	case processGone(err):
		s.emit(Event{Kind: ProcessExited, Process: ref})
		return nil
	case err != nil:
		s.deleteProc(ref.PID)
		return err
	case current.Start != ref.Start:
		s.deleteProc(ref.PID)
		s.emit(Event{Kind: ProcessExited, Process: ref})
		return nil
	}
	s.procs[ref.PID] = ref
	return nil
}

func (s *kqueueSource) deleteProc(pid int) {
	del := unix.Kevent_t{Ident: uint64(pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_DELETE} //nolint:gosec // G115: the PID is positive.
	_ = s.register(del)
}

// Unwatch removes the matching process incarnation, if it is watched.
func (s *kqueueSource) Unwatch(ref Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if known, ok := s.procs[ref.PID]; ok && known.Start == ref.Start {
		delete(s.procs, ref.PID)
		s.deleteProc(ref.PID)
	}
}

// WatchDir watches direct changes to a directory without following symlinks.
func (s *kqueueSource) WatchDir(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.dirs[path]; ok {
		return nil
	}
	fd, err := unix.Open(path, unix.O_EVTONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	add := unix.Kevent_t{
		Ident: uint64(fd), Filter: unix.EVFILT_VNODE, Flags: unix.EV_ADD | unix.EV_CLEAR, //nolint:gosec // G115: descriptors are non-negative.
		Fflags: unix.NOTE_WRITE | unix.NOTE_DELETE | unix.NOTE_RENAME | unix.NOTE_REVOKE,
	}
	if err := s.register(add); err != nil {
		_ = unix.Close(fd)
		return err
	}
	s.dirs[path], s.byFD[fd] = fd, path
	return nil
}

func (s *kqueueSource) loop() {
	defer s.wg.Done()
	buf := make([]unix.Kevent_t, 16)
	for {
		n, err := unix.Kevent(s.kq, nil, buf, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil {
			select {
			case <-s.done: // closed under us
			default:
				s.send(Event{Kind: SourceFailed})
			}
			return
		}
		for _, ev := range buf[:n] {
			switch ev.Filter {
			case unix.EVFILT_USER:
				return
			case unix.EVFILT_PROC:
				if ev.Fflags&unix.NOTE_EXIT != 0 {
					s.procExited(int(ev.Ident)) //nolint:gosec // G115: the identifier is a PID we registered.
				}
			case unix.EVFILT_VNODE:
				s.dirChanged(int(ev.Ident), ev.Fflags) //nolint:gosec // G115: the identifier is a descriptor we opened.
			}
		}
	}
}

func (s *kqueueSource) procExited(pid int) {
	s.mu.Lock()
	ref, ok := s.procs[pid]
	delete(s.procs, pid)
	s.mu.Unlock()
	if ok {
		s.send(Event{Kind: ProcessExited, Process: ref})
	}
}

func (s *kqueueSource) dirChanged(fd int, flags uint32) {
	if flags&(unix.NOTE_DELETE|unix.NOTE_RENAME|unix.NOTE_REVOKE) != 0 {
		// The directory is gone or moved: forget it so a recreated one can be watched.
		s.mu.Lock()
		if path, ok := s.byFD[fd]; ok {
			delete(s.byFD, fd)
			delete(s.dirs, path)
			_ = unix.Close(fd)
		}
		s.mu.Unlock()
	}
	s.send(Event{Kind: DirectoryChanged})
}

// Close stops notifications and releases OS handles; repeated calls are safe.
func (s *kqueueSource) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		trigger := unix.Kevent_t{Ident: kqueueWake, Filter: unix.EVFILT_USER, Fflags: unix.NOTE_TRIGGER}
		_, err = unix.Kevent(s.kq, []unix.Kevent_t{trigger}, nil, nil)
		s.wg.Wait()
		s.mu.Lock()
		for fd := range s.byFD {
			_ = unix.Close(fd)
		}
		s.dirs, s.byFD, s.procs = map[string]int{}, map[int]string{}, map[int]Identity{}
		s.mu.Unlock()
		if closeErr := unix.Close(s.kq); err == nil {
			err = closeErr
		}
	})
	return err
}
