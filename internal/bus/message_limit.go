package bus

import "github.com/zalkilabs/zalki/internal/model"

// checkAdmissionBody runs only after durable-ID reuse and before reservation.
// Persisted messages and their recovery/settlement retain the hard wire limit.
func checkAdmissionBody(body string, limit int) error {
	if limit == 0 {
		limit = model.MaxMessageBodyBytes
	}
	if limit < 1 || limit > model.MaxMessageBodyBytes {
		return model.NewError(model.CodeInvalidInput, "configured body limit must be between 1 and %d bytes", model.MaxMessageBodyBytes)
	}
	if len(body) > limit {
		return model.NewError(model.CodeInvalidInput, "body is %d UTF-8 bytes; configured maximum is %d bytes", len(body), limit)
	}
	return nil
}
