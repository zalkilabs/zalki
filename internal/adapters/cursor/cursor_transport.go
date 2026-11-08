package cursor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters/internal/socketio"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func signalCursor(ctx context.Context, message bus.Message, target bus.Session) bus.Receipt {
	receipt := model.NewReceipt(model.StatusPendingActivity)
	endpoint := &target.Listener.Endpoint
	if err := bus.CheckEndpoint(endpoint); err != nil {
		receipt.Error = "Cursor listener unavailable: " + err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	conn, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", endpoint.Path)
	if err != nil {
		receipt.Error = "Cursor listener unavailable: " + err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	defer conn.Close() //nolint:errcheck // The wake receipt is determined before connection cleanup.
	if err := bus.CheckEndpoint(endpoint); err != nil {
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
	receipt.Status = model.StatusWakeUncertain
	hint := wakeHint{MessageID: message.ID, Generation: target.Generation}
	if err := json.NewEncoder(conn).Encode(hint); err != nil {
		receipt.Error = err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	var response wakeHint
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&response); err != nil {
		receipt.Error = err.Error()
		receipt = receipt.WithCause(err)
		return receipt
	}
	if response != hint {
		receipt.Error = "Cursor listener returned a different message identity"
		return receipt
	}
	receipt.At, receipt.Status = model.NewReceipt(model.StatusWakeWritten).At, model.StatusWakeWritten
	return receipt
}
