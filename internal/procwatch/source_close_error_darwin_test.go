package procwatch

// A literal -1 cannot name another goroutine's descriptor. No producer is
// started; Close's unchanged syscall path returns EBADF without any real fd.
func closeErrorSource() Source {
	return &kqueueSource{kq: -1, events: make(chan Event), done: make(chan struct{})}
}
