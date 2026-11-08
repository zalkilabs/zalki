package bus

import (
	"log/slog"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestReceiptLogLevel(t *testing.T) {
	for status, want := range map[model.DeliveryStatus]slog.Level{
		model.StatusAttempting: slog.LevelInfo, model.StatusApprovalBlocked: slog.LevelInfo, model.StatusQueuedForApproval: slog.LevelInfo,
		model.StatusSubmitted: slog.LevelInfo, model.StatusTransportWritten: slog.LevelInfo, model.StatusWakeWritten: slog.LevelInfo,
		model.StatusPendingActivity: slog.LevelInfo, model.StatusContextEmitted: slog.LevelInfo,
		model.StatusNotSent: slog.LevelWarn, model.StatusCommandFailed: slog.LevelWarn, model.StatusUnknownTimeout: slog.LevelWarn,
		model.StatusUnknownWrite: slog.LevelWarn, model.StatusWakeUncertain: slog.LevelWarn,
	} {
		if got := receiptLogLevel(Receipt{Status: status}); got != want {
			t.Errorf("%s logged at %v, want %v", status, got, want)
		}
		if receiptLogLevel(Receipt{Status: status, Error: "adapter reported a problem"}) != slog.LevelWarn {
			t.Errorf("%s carrying an error must warn", status)
		}
	}
}
