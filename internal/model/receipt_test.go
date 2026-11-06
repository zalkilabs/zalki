// These in-package tests pin receipt values, live-cause identity and JSON.
// The bus tests independently verify the compatibility alias and transport path.
package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

func TestReceiptCauseAndJSON(t *testing.T) {
	original := Receipt{At: "at", Status: "submitted", Code: "unavailable", Error: "public error", Output: "output"}
	if original.Cause() != nil {
		t.Fatal("receipt starts with an unexpected cause")
	}
	cause := fmt.Errorf("private: %w", errors.New("diagnostic"))
	withCause := original.WithCause(cause)
	if got := withCause.Cause(); got != cause { //nolint:errorlint // The accessor must retain the exact supplied error identity.
		t.Fatal("WithCause lost its error identity")
	}
	if original.Cause() != nil {
		t.Fatal("WithCause changed the original receipt")
	}
	const want = `{"at":"at","status":"submitted","code":"unavailable","error":"public error","output":"output"}`
	encoded, err := json.Marshal(withCause)
	if err != nil || string(encoded) != want {
		t.Fatalf("encoded receipt=%s, error=%v, want %s", encoded, err, want)
	}
	var decoded Receipt
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Cause() != nil || decoded.At != "at" || decoded.Status != "submitted" || decoded.Code != "unavailable" || decoded.Error != "public error" || decoded.Output != "output" {
		t.Fatalf("decoded public fields or private cause changed: %+v", decoded)
	}
	cleared := withCause.WithCause(nil)
	if cleared.Cause() != nil {
		t.Fatal("WithCause(nil) did not clear the copy")
	}
	if got := withCause.Cause(); got != cause { //nolint:errorlint // Clearing the copy must preserve the original cause identity.
		t.Fatal("clearing a copy changed its source")
	}
}

func TestReceiptZeroJSONAndMethodContract(t *testing.T) {
	receipt := Receipt{}
	if receipt.Cause() != nil {
		t.Fatal("zero receipt has a cause")
	}
	encoded, err := json.Marshal(receipt)
	if err != nil || string(encoded) != `{"at":"","status":""}` {
		t.Fatalf("zero receipt=%s, error=%v", encoded, err)
	}
	if _, ok := any(receipt).(json.Marshaler); ok {
		t.Fatal("Receipt unexpectedly implements a custom JSON marshaler")
	}
	if _, ok := any(&receipt).(json.Marshaler); ok {
		t.Fatal("*Receipt unexpectedly implements a custom JSON marshaler")
	}
	if _, ok := any(receipt).(interface{ Unwrap() error }); ok {
		t.Fatal("Receipt unexpectedly unwraps a diagnostic cause")
	}
	if _, ok := any(&receipt).(interface{ Unwrap() error }); ok {
		t.Fatal("*Receipt unexpectedly unwraps a diagnostic cause")
	}
}

func TestNewReceiptPreservesStatusWithoutValidation(t *testing.T) {
	for _, status := range []string{"unknown_after_write", "future_status"} {
		t.Run(status, func(t *testing.T) {
			receipt := NewReceipt(DeliveryStatus(status))
			if string(receipt.Status) != status || receipt.Code != "" || receipt.Error != "" || receipt.Output != "" || receipt.Cause() != nil {
				t.Fatalf("NewReceipt changed the requested outcome or populated diagnostics: %+v", receipt)
			}
			requireReceiptClock(t, receipt.At)
		})
	}
}
