package cursor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
)

// wakeHint binds a native listener notification to a message and receiving
// registration. Peer text and credentials never enter this wire contract.
type wakeHint struct {
	MessageID  string `json:"message_id"`
	Generation string `json:"generation"`
}

// deliveryLease is the part of a core listener lease that hint handling uses.
type deliveryLease interface {
	Generation() string
	ValidateDelivery(messageID string) (acknowledged bool, err error)
	WakeEmitted(messageID string)
}

// ListenerHandle is the listener's view of a core-owned lease. The listener uses
// it from one goroutine; core retains authentication, fencing and journal IO.
type ListenerHandle interface {
	// Generation identifies the registration for which this handle was acquired.
	Generation() string
	// Arm binds the handle to the inspected endpoint.
	Arm(*bus.Endpoint) error
	// Confirm applies adapter state after readiness was announced. Core runs the
	// policy inside its journal transaction, so the policy must be fast and pure.
	Confirm(func(bus.SessionView) (json.RawMessage, error)) error
	// ValidateDelivery checks recipient, generation, pending state and prior ACK.
	ValidateDelivery(messageID string) (acknowledged bool, err error)
	// RejectHint records a rejected notification without exposing its contents.
	RejectHint(error)
	// WakeEmitted records output written for a validated message.
	WakeEmitted(messageID string)
	// Close releases this handle's lease and lifetime lock. It is idempotent.
	Close(cause error)
}

var errListenerOutput = errors.New("Cursor listener output failed")

// Listen must be started by the current Cursor model as one native Shell
// background task with notify_on_output matching "ZALKI_WAKE ". Only Cursor
// can register that native task; starting this from a hook would not wake a model.
// It blocks on socket accept and consumes no model turns while there is no mail.
// The caller supplies lease acquisition and endpoint inspection; neither callback
// changes the listener's lease lifecycle or owns its socket.
// Acquisition must return a usable handle on success; on error the listener
// ignores the handle. A failed acquisition owns no listener resources.
func Listen(ctx context.Context, acquire func(actor, key string) (ListenerHandle, error), inspect func(string) (*bus.Endpoint, error), actor, key string, output io.Writer) (resultErr error) {
	lease, err := acquire(actor, key)
	if err != nil {
		return err
	}
	defer func() { lease.Close(resultErr) }()
	// Keep below macOS's Unix-socket path limit, independent of long home/TMPDIR
	// paths. Each listener owns a fresh private directory and removes only that.
	dir, err := os.MkdirTemp("/tmp", "zalki-cursor-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir) //nolint:errcheck // Best-effort cleanup of this listener's private temporary directory.
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "inbox.sock"), Net: "unix"})
	if err != nil {
		return err
	}
	defer listener.Close() //nolint:errcheck // Cancellation may already have closed the listener.
	if err := os.Chmod(listener.Addr().String(), 0600); err != nil {
		return err
	}
	endpoint, err := inspect(listener.Addr().String())
	if err != nil {
		return err
	}
	if err := lease.Arm(endpoint); err != nil {
		return err
	}
	stopClose := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopClose()
	if _, err := fmt.Fprintln(output, "ZALKI_LISTENER_READY", actor); err != nil {
		return err
	}
	// A process that cannot write to its native task output never completed
	// setup. Keep the recovery guard in that case instead of retrying forever.
	if err := lease.Confirm(listenerConfirmed); err != nil {
		return err
	}
	for {
		conn, err := listener.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		err = consumeHint(conn, lease, output)
		_ = conn.Close() // The wake attempt has already completed.
		if errors.Is(err, errListenerOutput) {
			return err
		}
		if err != nil {
			lease.RejectHint(err)
		}
		// Invalid or stale hints cannot terminate the valid listener, and neither
		// their content nor their errors are printed into the model's inbox.
	}
}

// listenerConfirmed resets the recovery guard once a listener is announced.
func listenerConfirmed(v bus.SessionView) (json.RawMessage, error) {
	state := decodeCursorState(v.AdapterState)
	state.RecoveryAttempted = false
	return encodeCursorState(state), nil
}

func consumeHint(conn net.Conn, lease deliveryLease, output io.Writer) error {
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	var hint wakeHint
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&hint); err != nil {
		return err
	}
	if hint.Generation != lease.Generation() {
		return errors.New("stale Cursor wake generation")
	}
	acknowledged, err := lease.ValidateDelivery(hint.MessageID)
	if err != nil {
		return err
	}
	if !acknowledged {
		// Only validated IDs enter notifications; peer text stays in the journal.
		if _, err := fmt.Fprintln(output, "ZALKI_WAKE", hint.MessageID); err != nil {
			return fmt.Errorf("%w: %w", errListenerOutput, err)
		}
		lease.WakeEmitted(hint.MessageID)
	}
	return json.NewEncoder(conn).Encode(hint)
}
