package bus

import (
	"errors"
	"os"
)

// cursorPrompt observes an actual prompt in an opted-in conversation. Cursor may
// resume the same conversation without another sessionStart; retain its identity
// and capability instead of leaving the previous sessionEnd retirement sticky.
// Background tool/stop events must not reactivate a retired conversation.
func (s Store) cursorPrompt(id string) error {
	return s.locked(func() error {
		current, err := s.session(id)
		if errors.Is(err, os.ErrNotExist) {
			return nil // Startup must supply the initial identity and capability.
		}
		if err != nil {
			return err
		}
		if current.Kind != "cursor" {
			return errors.New("Cursor prompt requires a Cursor session")
		}
		current.State, current.UpdatedAt, current.Retired = StateBusy, timestamp(), false
		current.RetiredReason = ""
		state := cursorState(current)
		if state.RecoverySuppressed {
			state.RecoveryAttempted = false
		}
		state.RecoverySuppressed = false
		setCursorState(&current, state)
		return s.saveSession(current)
	})
}
