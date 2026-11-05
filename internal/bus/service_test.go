package bus

import (
	"log/slog"
	"testing"
)

func TestReceiptLogLevel(t *testing.T) {
	for status, want := range map[DeliveryStatus]slog.Level{
		StatusAttempting: slog.LevelInfo, StatusApprovalBlocked: slog.LevelInfo, StatusQueuedForApproval: slog.LevelInfo,
		StatusSubmitted: slog.LevelInfo, StatusTransportWritten: slog.LevelInfo, StatusWakeWritten: slog.LevelInfo,
		StatusPendingActivity: slog.LevelInfo, StatusContextEmitted: slog.LevelInfo,
		StatusNotSent: slog.LevelWarn, StatusCommandFailed: slog.LevelWarn, StatusUnknownTimeout: slog.LevelWarn,
		StatusUnknownWrite: slog.LevelWarn, StatusWakeUncertain: slog.LevelWarn,
	} {
		if got := receiptLogLevel(Receipt{Status: status}); got != want {
			t.Errorf("%s logged at %v, want %v", status, got, want)
		}
		if receiptLogLevel(Receipt{Status: status, Error: "adapter reported a problem"}) != slog.LevelWarn {
			t.Errorf("%s carrying an error must warn", status)
		}
	}
}
