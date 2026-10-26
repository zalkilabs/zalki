package socketio

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

type blockedClose struct {
	net.Conn
	started, unblock chan struct{}
}

func (c blockedClose) Close() error {
	close(c.started)
	<-c.unblock
	return c.Conn.Close()
}

func TestCleanupJoinsCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn := blockedClose{client, make(chan struct{}), make(chan struct{})}
	var once sync.Once
	unblock := func() { once.Do(func() { close(conn.unblock) }) }
	defer unblock()
	release, err := BindContext(ctx, conn, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-conn.started:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close the socket")
	}
	cleaned := make(chan struct{})
	go func() { release(); close(cleaned) }()
	select {
	case <-cleaned:
		t.Fatal("cleanup returned while cancellation still owned the connection")
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	select {
	case <-cleaned:
	case <-time.After(time.Second):
		t.Fatal("cleanup did not join the finished cancellation callback")
	}
}

func TestCleanupStopsLaterCancellation(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	release, err := BindContext(ctx, client, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	release()
	cancel()
	// After releasing ownership, later caller cancellation must not close a
	// connection which is now owned by the transport's normal cleanup path.
	written := make(chan error, 1)
	go func() { _, err := client.Write([]byte("x")); written <- err }()
	if err := server.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Read(make([]byte, 1)); err != nil {
		t.Fatal("released callback still closed the connection", err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
}
