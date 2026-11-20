package pluginfeed

import (
	"context"
	"errors"
	"log/slog"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type streamIntegrity struct{ id, reason string }
type streamWaiting struct {
	reason string
	delay  int
}

func (s PluginFeed) noteStreamReadFailure(id string, cause error) {
	reason := "record_unreadable"
	if errors.Is(cause, journal.ErrInvalidBatch) {
		reason = "invalid_record"
	}
	if errors.Is(cause, journal.ErrInvalidFile) {
		reason = "record_file_rejected"
	}
	s.noteStreamIntegrity(id, reason, cause)
}

func (s PluginFeed) noteStreamIntegrity(id, reason string, cause error) {
	if errors.Is(cause, errStreamWork) || errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || s.stream.integrityEpisode {
		return
	}
	if !model.IsCanonicalUUID(id) {
		id = "unknown"
	}
	s.stream.integrityEpisode = true
	s.stream.integrityPending = &streamIntegrity{id: id, reason: reason}
}

func (s PluginFeed) finishStreamStep() {
	pending := s.stream.integrityPending
	waiting := s.stream.waitingPending
	s.stream.integrityPending = nil
	s.stream.waitingPending = nil
	s.stream.mu.Unlock()
	if s.store.Logger() == nil {
		return
	}
	// Both the Store journal view and the runtime mutex have ended before
	// invoking a diagnostic sink. Never serialize errors or domain records.
	if pending != nil {
		s.store.Logger().LogAttrs(context.Background(), slog.LevelInfo, "plugin.feed.integrity", slog.String("event_id", pending.id), slog.String("reason", pending.reason))
	}
	if waiting != nil {
		s.store.Logger().LogAttrs(context.Background(), slog.LevelInfo, "plugin.feed.waiting", slog.String("reason", waiting.reason), slog.Int("retry_delay_seconds", waiting.delay))
	}
}
