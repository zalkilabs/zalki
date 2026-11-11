package bus

import (
	"encoding/json"
	"errors"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// sessionEvent projects only registration, generation and lifecycle facts.
func sessionEvent(previous []byte, next model.Session) (model.Event, error) {
	kind, projection, err := sessionProjection(previous, next)
	if err != nil || kind == "" {
		return model.Event{}, err
	}
	event := model.Event{ID: newID(), Type: kind, OccurredAt: model.Timestamp(), TransactionID: newID(), Session: projection}
	return event, event.Validate()
}

func sessionProjection(previous []byte, next model.Session) (string, model.EventSession, error) {
	var before model.Session
	if previous != nil {
		if err := json.Unmarshal(previous, &before); err != nil {
			return "", model.EventSession{}, err
		}
		if err := checkSchema(previous, leaseSessionSchema); err != nil {
			return "", model.EventSession{}, err
		}
		if before.ID != next.ID {
			return "", model.EventSession{}, journal.ErrInvalidBatch
		}
	}
	kind := "session.state_changed"
	if previous == nil || before.Generation != next.Generation {
		kind = "session.registered"
	} else if before.State == next.State && before.Retired == next.Retired {
		return "", model.EventSession{}, nil
	}
	return kind, model.EventSession{ID: next.ID, Generation: next.Generation, Previous: before.State, State: next.State,
		PreviousRetired: before.Retired, Retired: next.Retired}, nil
}

func validatePreparedSession(previous []byte, next model.Session, event model.Event) error {
	kind, projection, err := sessionProjection(previous, next)
	if err != nil {
		return err
	}
	if kind != event.Type || projection != event.Session {
		return errors.New("event does not match session change")
	}
	return event.Validate()
}
