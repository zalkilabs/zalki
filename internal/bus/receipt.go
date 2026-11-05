package bus

// NewReceipt starts a receipt for status, stamped with the current time.
func NewReceipt(status DeliveryStatus) Receipt {
	return Receipt{At: timestamp(), Status: status}
}

// WithCause returns r with a live diagnostic cause attached. The cause is never
// serialized, persisted, or copied into the public Error text.
func (r Receipt) WithCause(cause error) Receipt {
	r.cause = cause
	return r
}

type Receipt struct {
	cause  error          // Live diagnostic category only; never serialized or persisted.
	At     string         `json:"at"`
	Status DeliveryStatus `json:"status"`
	Code   ErrorCode      `json:"code,omitempty"`
	Error  string         `json:"error,omitempty"`
	Output string         `json:"output,omitempty"`
}
