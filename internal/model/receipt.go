package model

// Receipt records a transport outcome and its public diagnostic fields.
// Its live cause is retained only in memory and never serialized or persisted.
type Receipt struct {
	cause error
	// At is the receipt's timestamp.
	At string `json:"at"`
	// Status is the transport-reported outcome.
	Status DeliveryStatus `json:"status"`
	// Code classifies a public delivery error when present.
	Code ErrorCode `json:"code,omitempty"`
	// Error contains the public diagnostic text when present.
	Error string `json:"error,omitempty"`
	// Output contains public native output when present.
	Output string `json:"output,omitempty"`
}

// NewReceipt starts a receipt for status with one current timestamp sample.
// It constructs a value; it does not validate or apply a delivery transition.
func NewReceipt(status DeliveryStatus) Receipt {
	return Receipt{At: Timestamp(), Status: status}
}

// WithCause returns r with a live diagnostic cause attached. The cause is never
// serialized, persisted, or copied into the public Error text. It changes only
// the returned value, not r or any persisted delivery state.
func (r Receipt) WithCause(cause error) Receipt {
	r.cause = cause
	return r
}

// Cause returns the live diagnostic cause without changing receipt state.
// This accessor does not unwrap the receipt or expose the cause in JSON.
func (r Receipt) Cause() error { return r.cause }
