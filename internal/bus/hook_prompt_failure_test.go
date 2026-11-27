package bus

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

// The failed-registration line points to the log only when the
// log records warnings; otherwise it says that the cause was not saved.
func TestPromptRegistrationFailureNamesWhereTheCauseIs(t *testing.T) {
	const failed = "Zalki could not safely register this chat. Your message was not blocked."
	off := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	on := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo}))
	for _, tc := range []struct {
		name   string
		logger *slog.Logger
		want   string
	}{
		{"no logger", nil, failed + " Logging is off, so the cause was not saved; zalki config show shows the logging settings."},
		{"warnings not logged", off, failed + " Logging is off, so the cause was not saved; zalki config show shows the logging settings."},
		{"warnings logged", on, failed + " The cause is in the zalki log; zalki config show prints its path."},
	} {
		got := Store{logger: tc.logger}.promptRegistrationFailed(false, "")
		if got != tc.want || strings.Contains(got, "doctor") {
			t.Fatalf("%s: %q", tc.name, got)
		}
	}
}
