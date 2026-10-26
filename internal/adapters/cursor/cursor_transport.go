package cursor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters/internal/socketio"
	"github.com/zalkilabs/zalki/internal/bus"
)

func signalCursor(ctx context.Context, message bus.Message, target bus.Session) bus.Receipt {
	receipt := bus.NewReceipt(bus.StatusPendingActivity)
	if err := bus.CheckEndpoint(cursorState(target).Wake); err != nil {
		receipt.Error = "Cursor listener unavailable: " + err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", cursorState(target).Wake.Path)
	if err != nil {
		receipt.Error = "Cursor listener unavailable: " + err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	defer conn.Close() //nolint:errcheck // The wake receipt is determined before connection cleanup.
	if err := bus.CheckEndpoint(cursorState(target).Wake); err != nil {
		receipt.Error = err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	release, err := socketio.BindContext(ctx, conn, 3*time.Second)
	if err != nil {
		receipt.Error = err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	defer release()
	if err := ctx.Err(); err != nil {
		receipt.Error = err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	receipt.Status = bus.StatusWakeUncertain
	hint := bus.CursorWakeHint{MessageID: message.ID, Generation: target.Generation}
	if err := json.NewEncoder(conn).Encode(hint); err != nil {
		receipt.Error = err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	var response bus.CursorWakeHint
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&response); err != nil {
		receipt.Error = err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	if response != hint {
		receipt.Error = "Cursor listener returned a different message identity"
		return receipt
	}
	receipt.At, receipt.Status = bus.NewReceipt(bus.StatusWakeWritten).At, bus.StatusWakeWritten
	return receipt
}
