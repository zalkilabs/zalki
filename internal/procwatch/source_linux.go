//go:build linux

package procwatch

import (
	"encoding/binary"
	"errors"
	"sync"
	"syscall"

	"golang.org/x/sys/unix"
)

// pidfdSource implements Source with pidfd (a descriptor that becomes
// readable when its process exits) and inotify for directory changes, both
// multiplexed through one epoll descriptor.
type pidfdSource struct {
	epfd, inotify int
	wakeR, wakeW  int
	mu            sync.Mutex
	pidfds        map[int]Identity // pidfd -> ref
	byKey         map[Identity]int // ref -> pidfd
	wds           map[int]string   // inotify watch -> path
	paths         map[string]int
	events        chan Event
	done          chan struct{}
	once          sync.Once
	wg            sync.WaitGroup
}

const inotifyMask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MOVED_FROM | unix.IN_MOVED_TO |
	unix.IN_CLOSE_WRITE | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF | unix.IN_ONLYDIR | unix.IN_DONT_FOLLOW

// NewSource opens the platform process and directory notification source.
func NewSource() (Source, error) {
	// Probe pidfd support first: a kernel before 5.3 gets the sweep alone.
	probe, err := unix.PidfdOpen(1, 0)
	if err != nil && errors.Is(err, syscall.ENOSYS) {
		return nil, ErrUnsupported
	}
	if err == nil {
		_ = unix.Close(probe)
	}
	s := &pidfdSource{
		pidfds: map[int]Identity{}, byKey: map[Identity]int{}, wds: map[int]string{}, paths: map[string]int{},
		events: make(chan Event, 64), done: make(chan struct{}),
	}
	closeAll := func(err error) (Source, error) {
		for _, fd := range []int{s.epfd, s.inotify, s.wakeR, s.wakeW} {
			if fd > 0 {
				_ = unix.Close(fd)
			}
		}
		return nil, err
	}
	if s.epfd, err = unix.EpollCreate1(unix.EPOLL_CLOEXEC); err != nil {
		return closeAll(err)
	}
	if s.inotify, err = unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK); err != nil {
		return closeAll(err)
	}
	var pipe [2]int
	if err = unix.Pipe2(pipe[:], unix.O_CLOEXEC|unix.O_NONBLOCK); err != nil {
		return closeAll(err)
	}
	s.wakeR, s.wakeW = pipe[0], pipe[1]
	for _, fd := range []int{s.inotify, s.wakeR} {
		if err = s.epollAdd(fd); err != nil {
			return closeAll(err)
		}
	}
	s.wg.Add(1)
	go s.loop()
	return s, nil
}

func (s *pidfdSource) epollAdd(fd int) error {
	return unix.EpollCtl(s.epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(fd)}) //nolint:gosec // G115: descriptors are non-negative and small.
}

// Events returns the raw notifications produced by this source.
func (s *pidfdSource) Events() <-chan Event { return s.events }

// emit never blocks the caller; the event is dropped once the source is closed.
func (s *pidfdSource) emit(ev Event) { go s.send(ev) }

func (s *pidfdSource) send(ev Event) {
	select {
	case s.events <- ev:
	case <-s.done:
	}
}

// WatchProcess watches one process incarnation without waiting for a consumer.
func (s *pidfdSource) WatchProcess(ref Identity) error {
	if ref.PID <= 1 {
		return errors.New("invalid process id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byKey[ref]; ok {
		return nil
	}
	fd, err := unix.PidfdOpen(ref.PID, 0)
	if errors.Is(err, syscall.ESRCH) {
		s.emit(Event{Kind: ProcessExited, Process: ref})
		return nil
	}
	if err != nil {
		return err
	}
	// Verify after opening: the pidfd pins the incarnation we check, so a reused
	// pid is caught here rather than attached to the wrong process.
	current, err := Read(ref.PID)
	switch {
	case processGone(err) || (err == nil && current.Start != ref.Start):
		_ = unix.Close(fd)
		s.emit(Event{Kind: ProcessExited, Process: ref})
		return nil
	case err != nil:
		_ = unix.Close(fd)
		return err
	}
	if err := s.epollAdd(fd); err != nil {
		_ = unix.Close(fd)
		return err
	}
	s.pidfds[fd], s.byKey[ref] = ref, fd
	return nil
}

// Unwatch removes the matching process incarnation, if it is watched.
func (s *pidfdSource) Unwatch(ref Identity) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fd, ok := s.byKey[ref]; ok {
		s.dropProcess(fd)
	}
}

// dropProcess forgets and closes a pidfd. The caller holds s.mu.
func (s *pidfdSource) dropProcess(fd int) {
	if ref, ok := s.pidfds[fd]; ok {
		delete(s.byKey, ref)
		delete(s.pidfds, fd)
		_ = unix.EpollCtl(s.epfd, unix.EPOLL_CTL_DEL, fd, nil)
		_ = unix.Close(fd)
	}
}

// WatchDir watches direct changes to a directory without following symlinks.
func (s *pidfdSource) WatchDir(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.paths[path]; ok {
		return nil
	}
	wd, err := unix.InotifyAddWatch(s.inotify, path, inotifyMask)
	if err != nil {
		return err
	}
	s.wds[wd], s.paths[path] = path, wd
	return nil
}

func (s *pidfdSource) loop() {
	defer s.wg.Done()
	buf := make([]unix.EpollEvent, 16)
	for {
		n, err := unix.EpollWait(s.epfd, buf, -1)
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
			switch fd := int(ev.Fd); fd {
			case s.wakeR:
				return
			case s.inotify:
				s.drainInotify()
			default:
				s.procExited(fd)
			}
		}
	}
}

func (s *pidfdSource) procExited(fd int) {
	s.mu.Lock()
	ref, ok := s.pidfds[fd]
	if ok {
		s.dropProcess(fd)
	}
	s.mu.Unlock()
	if ok {
		s.send(Event{Kind: ProcessExited, Process: ref})
	}
}

// drainInotify reads every queued record. Any change is reported once; the
// observer re-reads the directories, so which entry changed does not matter.
func (s *pidfdSource) drainInotify() {
	buf := make([]byte, 4096)
	changed := false
	for {
		n, err := unix.Read(s.inotify, buf)
		if err != nil || n <= 0 {
			break
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			wd := int(int32(binary.NativeEndian.Uint32(buf[off:]))) //nolint:gosec // G115: kernel watch descriptor.
			mask := binary.NativeEndian.Uint32(buf[off+4:])
			length := int(binary.NativeEndian.Uint32(buf[off+12:]))
			off += unix.SizeofInotifyEvent + length
			changed = true
			if mask&unix.IN_IGNORED != 0 {
				// The directory is gone: forget it so a recreated one can be watched.
				s.mu.Lock()
				if path, ok := s.wds[wd]; ok {
					delete(s.wds, wd)
					delete(s.paths, path)
				}
				s.mu.Unlock()
			}
		}
	}
	if changed {
		s.send(Event{Kind: DirectoryChanged})
	}
}

// Close stops notifications and releases OS handles; repeated calls are safe.
func (s *pidfdSource) Close() error {
	var err error
	s.once.Do(func() {
		close(s.done)
		_, err = unix.Write(s.wakeW, []byte{1})
		s.wg.Wait()
		s.mu.Lock()
		for fd := range s.pidfds {
			_ = unix.Close(fd)
		}
		s.pidfds, s.byKey, s.wds, s.paths = map[int]Identity{}, map[Identity]int{}, map[int]string{}, map[string]int{}
		s.mu.Unlock()
		for _, fd := range []int{s.inotify, s.wakeR, s.wakeW, s.epfd} {
			if closeErr := unix.Close(fd); err == nil {
				err = closeErr
			}
		}
	})
	return err
}
