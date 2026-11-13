package bus

import "github.com/zalkilabs/zalki/internal/model"

// holdTransportApproval persists a positive native approval rejection only for
// the same still-open attempt. A concurrent durable outcome always wins.
func (s *Service) holdTransportApproval(attempt model.Message) (result model.Message, resultErr error) {
	newlyHeld := false
	resultErr = s.Store.locked(func() error {
		stored, err := s.Store.message(attempt.ID)
		if err != nil {
			return err
		}
		if !sameTransportApprovalAttempt(stored, attempt) {
			return model.NewError(model.CodeConflict, "message changed during native approval check")
		}
		result = stored
		if !transportApprovalAttemptOpen(stored) {
			return nil
		}
		if err := validateTransportApprovalTarget(s.Store, stored); err != nil {
			result, err = settleTransportApprovalRefusal(s.Store, stored, err)
			return err
		}
		candidate := stored
		if err := s.Store.holdForApproval(&candidate); err != nil {
			result, err = settleTransportApprovalRefusal(s.Store, stored, err)
			return err
		}
		if err := s.Store.saveReservation(candidate); err != nil {
			return err
		}
		result = candidate
		newlyHeld = true
		return nil
	})
	if resultErr == nil && newlyHeld {
		s.Store.logReservation(result)
	}
	return result, resultErr
}

func sameTransportApprovalAttempt(stored, attempted model.Message) bool {
	return stored.ID == attempted.ID && stored.From == attempted.From && stored.To == attempted.To &&
		stored.TargetGeneration == attempted.TargetGeneration && stored.Body == attempted.Body &&
		stored.ReplyTo == attempted.ReplyTo && stored.Kind == attempted.Kind
}

func transportApprovalAttemptOpen(msg model.Message) bool {
	return msg.Status == model.StatusAttempting && !msg.Held && msg.AcknowledgedAt == "" && msg.Receipt == nil
}

func validateTransportApprovalTarget(store Store, msg model.Message) error {
	if err := checkPluginMessageDelivery(store, msg); err != nil {
		return err
	}
	current, err := store.session(msg.To)
	if err != nil {
		return err
	}
	if current.Retired || current.Generation != msg.TargetGeneration {
		return model.NewError(model.CodeUnavailable, "destination registration changed before delivery")
	}
	if store.sessionLiveness(current) == model.LivenessExited {
		return model.NewError(model.CodeUnavailable, "destination owner process exited before delivery")
	}
	return nil
}

// The caller has proved this is the same open attempt and holds the journal
// lock, so this refusal cannot overwrite a racing ACK, final receipt or hold.
func settleTransportApprovalRefusal(store Store, msg model.Message, cause error) (model.Message, error) {
	if err := msg.SettleNotSent(model.ErrorCodeOf(cause), cause.Error()); err != nil {
		return model.Message{}, err
	}
	if err := store.saveMessage(msg); err != nil {
		return model.Message{}, err
	}
	return msg, nil
}
