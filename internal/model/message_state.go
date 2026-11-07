package model

// Transition evaluates a delivery transition without changing a message.
// Attempts accept delivery receipts; pending states may emit hook context.
// Emitted context wins a racing wake receipt. Only a held approval may return
// to attempting/pending or settle not-sent; legacy refusals cannot reopen.
func Transition(from, to DeliveryStatus, held bool) (DeliveryStatus, error) {
	if held && from == StatusApprovalBlocked && (to == StatusAttempting || to == StatusPendingActivity || to == StatusNotSent) {
		return to, nil
	}
	if !to.Valid() || to == StatusAttempting {
		return from, NewError(CodeInvalidTransition, "invalid delivery receipt %q", to)
	}
	// Hook context may be emitted while a wake transport is completing.
	if from == StatusContextEmitted && (HookPendingStatus(to) || to == from) {
		return from, nil
	}
	if from == to || from == StatusAttempting || (HookPendingStatus(from) && (HookPendingStatus(to) || to == StatusContextEmitted)) {
		return to, nil
	}
	return from, NewError(CodeInvalidTransition, "cannot change delivery status from %q to %q", from, to)
}

// ApprovalQueuedNote is the persisted public explanation for a held message.
const ApprovalQueuedNote = "queued until the permission prompt is answered"

// IsQueuedForApproval distinguishes a held message from a legacy final refusal.
// The derived queued_for_approval view is never written to the journal.
func (msg Message) IsQueuedForApproval() bool {
	return msg.Held && msg.Status == StatusApprovalBlocked
}

func (msg *Message) move(to DeliveryStatus, held bool) error {
	next, err := Transition(msg.Status, to, held)
	if err != nil {
		return err
	}
	msg.Status = next
	return nil
}

// Hold applies the approval-held transition to this in-memory message.
// A refused transition leaves the message unchanged.
func (msg *Message) Hold() error {
	if err := msg.move(StatusApprovalBlocked, false); err != nil {
		return err
	}
	msg.Held = true
	msg.Receipt = &Receipt{At: Timestamp(), Status: StatusApprovalBlocked, Error: ApprovalQueuedNote}
	return nil
}

// SettleNotSent applies a final refusal and replaces its in-memory receipt.
// It clears a hold only after the transition succeeds; it does not persist.
func (msg *Message) SettleNotSent(code ErrorCode, reason string) error {
	if err := msg.move(StatusNotSent, msg.IsQueuedForApproval()); err != nil {
		return err
	}
	msg.Held = false
	msg.Receipt = &Receipt{At: Timestamp(), Status: StatusNotSent, Code: code, Error: reason}
	return nil
}

// ReleaseHeld reopens a held approval for the allowed attempt or hook state.
// It clears the prior receipt only after validation and the transition succeed.
func (msg *Message) ReleaseHeld(to DeliveryStatus) error {
	if !msg.IsQueuedForApproval() || (to != StatusAttempting && to != StatusPendingActivity) {
		return NewError(CodeInvalidTransition, "only a held message may return to attempting or pending delivery")
	}
	if err := msg.move(to, true); err != nil {
		return err
	}
	msg.Held = false
	msg.Receipt = nil
	if to == StatusPendingActivity {
		msg.Receipt = &Receipt{At: Timestamp(), Status: to}
	}
	return nil
}

// EmitContext records emitted hook context in this in-memory message.
// It replaces the receipt only after the transition succeeds.
func (msg *Message) EmitContext() error {
	if err := msg.move(StatusContextEmitted, false); err != nil {
		return err
	}
	msg.Receipt = &Receipt{At: Timestamp(), Status: StatusContextEmitted}
	return nil
}

// ApplyReceipt applies a transport outcome to this in-memory message.
// It validates the transition before filling a missing diagnostic code and
// replacing the receipt; it does not write storage or acknowledge the message.
func (msg *Message) ApplyReceipt(receipt Receipt) error {
	if err := msg.move(receipt.Status, false); err != nil {
		return err
	}
	if receipt.Error != "" && receipt.Code == "" {
		receipt.Code = CodeUnavailable
	}
	msg.Receipt = &receipt
	return nil
}
