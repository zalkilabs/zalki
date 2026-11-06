// These in-package tests pin the fixed UTC clock encoding without a clock hook.
package model

import (
	"regexp"
	"testing"
	"time"
)

var receiptClockPattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z$`)

func requireReceiptClock(t *testing.T, at string) {
	t.Helper()
	if !receiptClockPattern.MatchString(at) {
		t.Fatalf("timestamp %q must have nine fractional digits and a UTC Z suffix", at)
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000000000Z", at); err != nil {
		t.Fatalf("timestamp is not a valid calendar time: %v", err)
	}
}

func TestTimestampUsesCurrentUTCClock(t *testing.T) {
	before := time.Now().UTC()
	stamp := Timestamp()
	after := time.Now().UTC()
	requireReceiptClock(t, stamp)
	parsed, err := time.Parse("2006-01-02T15:04:05.000000000Z", stamp)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Before(before) || parsed.After(after) {
		t.Fatalf("Timestamp()=%q is outside the calling interval %s..%s", stamp, before, after)
	}
}
