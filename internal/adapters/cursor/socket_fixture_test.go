package cursor

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
)

func listenCursorSocket(t *testing.T) (*net.UnixListener, *bus.Endpoint) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "zalki-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "receiver.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if errors.Is(err, syscall.EPERM) {
		t.Skip("execution sandbox blocks Unix socket bind")
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	endpoint, err := bus.InspectEndpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return listener, endpoint
}
