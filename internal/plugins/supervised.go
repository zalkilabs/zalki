package plugins

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// maxcontrolFrame bounds one host-to-supervisor frame.
const maxcontrolFrame = 512

// controlFrame is the private metadata a supervised host reports to its
// supervisor on the control pipe, one JSON line each. It never carries a
// capability, message text, stderr, argv or config, and it is not the plugin
// wire. Phase is "ready" (handshake done, current binding) or "stopped".
type controlFrame struct {
	Installation string `json:"installation,omitempty"`
	Epoch        string `json:"epoch,omitempty"`
	Revision     string `json:"revision,omitempty"`
	Phase        string `json:"phase"`
	HostPID      int    `json:"host_pid"`
	PluginPID    int    `json:"plugin_pid,omitempty"`
	Reason       string `json:"reason,omitempty"`
}

type supervision struct {
	reason    string // set when the stop reason is already known (pin_invalid)
	file      *os.File
	binding   *pluginfeed.PluginFeedStatus
	pluginPID int
}

// bind and started are no-ops for a manual (unsupervised) run.
func (s *supervision) bind(binding *pluginfeed.PluginFeedStatus) {
	if s != nil {
		s.binding = binding
	}
}

func (s *supervision) started(pluginPID int) {
	if s != nil {
		s.pluginPID = pluginPID
	}
}

func (s *supervision) send(phase, reason string) error {
	frame := controlFrame{Phase: phase, HostPID: os.Getpid(), PluginPID: s.pluginPID, Reason: reason}
	if s.binding != nil {
		frame.Installation, frame.Epoch, frame.Revision = s.binding.ID, s.binding.Epoch, s.binding.Revision
	}
	raw, err := json.Marshal(frame)
	if err != nil || len(raw) >= maxcontrolFrame {
		return errors.New("supervised control frame is too large")
	}
	if err := s.file.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err = s.file.Write(append(raw, '\n'))
	return err
}

// readyPipe turns the host's single ready line into one ready frame,
// through a standard pipe: nothing is printed. stop closes the pipe and
// waits for its reader, so the ready frame always precedes "stopped".
func (s *supervision) readyPipe() (io.Writer, func()) {
	reader, writer := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := bufio.NewReader(reader).ReadString('\n'); err == nil {
			_ = s.send("ready", "")
		}
		_, _ = io.Copy(io.Discard, reader)
	}()
	return writer, func() {
		_ = writer.Close()
		<-done
	}
}

// prepare is prepareLaunch for a host. A pre-start pin failure (digest,
// argv or file type) is a permanent stop reason in supervised mode; its
// text is unchanged. It is a no-op addition for a manual run.
func (s *supervision) prepare(status pluginfeed.PluginFeedStatus) (pluginfeed.PluginLaunch, fileIdentity, string, error) {
	launch, pinned, ready, err := prepareLaunch(status)
	if err != nil && s != nil {
		s.reason = "pin_invalid"
	}
	return launch, pinned, ready, err
}

// stopCode is the reason for the stopped frame: an override set on the
// way, else the classified result.
func (s *supervision) stopCode(err error) string {
	if s.reason != "" {
		return s.reason
	}
	return stopReason(err)
}

// stopReason maps a host result to a controlled reason code; the error text
// itself never leaves the host.
func stopReason(err error) string {
	var hint hostHintError
	switch {
	case err == nil:
		return "exited"
	case errors.Is(err, errPinChangedAtStart):
		return "pin_changed"
	case errors.Is(err, syscall.EWOULDBLOCK):
		return "lease_held"
	case errors.Is(err, errHelloVersion):
		return "unsupported_wire"
	case errors.As(err, &hint):
		return "refused"
	}
	return "failed"
}
