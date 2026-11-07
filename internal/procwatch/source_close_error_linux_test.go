package procwatch

// Every descriptor is literal -1; none is zero, positive or a previously
// closed descriptor that the OS could have reused. There is no producer.
func closeErrorSource() Source {
	return &pidfdSource{epfd: -1, inotify: -1, wakeR: -1, wakeW: -1, events: make(chan Event), done: make(chan struct{})}
}
