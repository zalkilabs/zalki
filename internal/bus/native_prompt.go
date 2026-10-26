package bus

import (
	"errors"
	"log/slog"
	"os"
	"strings"
)

// completedNativePrompt filters only an exact, journal-backed envelope addressed
// to this conversation. Mentions, quoted messages, altered bodies and normal
// owner prompts are never blocked just because they contain a message UUID.
func (s Store) completedNativePrompt(actor, prompt string) (string, error) {
	const prefix = "[zalki message "
	if !strings.HasPrefix(prompt, prefix) {
		return "", nil
	}
	firstLine, _, ok := strings.Cut(prompt, "\n")
	if !ok || !strings.HasSuffix(firstLine, "]") {
		return "", nil
	}
	id := strings.TrimSuffix(strings.TrimPrefix(firstLine, prefix), "]")
	if _, err := UUID(id); err != nil {
		return "", nil
	}
	var reason string
	err := s.locked(func() error {
		message, err := s.message(id)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		expected := Envelope(s.withLabel(message))
		if message.To != actor || prompt != expected {
			return nil
		}
		current, err := s.session(actor)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if message.TargetGeneration != current.Generation {
			reason = "Zalki queued message belongs to a previous registration; not replayed."
		} else if message.AcknowledgedAt != "" {
			reason = "Zalki queued message already acknowledged; no additional model turn is needed."
		}
		if reason != "" {
			s.logEvent(slog.LevelDebug, "message.native_filtered", messageEvent(message))
		}
		return nil
	})
	return reason, err
}
