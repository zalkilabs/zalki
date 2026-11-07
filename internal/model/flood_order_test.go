package model

import (
	"math"
	"strings"
	"testing"
	"time"
)

// admitWith runs admit on a fresh state that the test has changed.
func admitWith(change func(state *FloodState, now time.Time), sender, id string) (FloodState, bool, error) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	state := newFloodAdmissionState(now)
	change(&state, now)
	changed, err := applyFloodAdmission(&state, sender, id, now)
	return state, changed, err
}

func arrivals(now time.Time, count int) []FloodArrival {
	list := make([]FloodArrival, count)
	for i := range list {
		list[i] = FloodArrival{ID: "old-" + string(rune('a'+i%26)) + "-" + time.Duration(i).String(), Sender: "other", At: now.Add(-time.Duration(count-i) * time.Second)}
	}
	return list
}

// admitCase is one invalid or refused state for admit.
type admitCase struct {
	name     string
	change   func(state *FloodState, now time.Time)
	id       string
	wantCode ErrorCode
	wantText string
}

const (
	tooManyArrivals = "invalid hourly flood quota state"
	badArrivalTime  = "invalid flood quota arrival time"
	badSenderBucket = "invalid sender flood quota state"
	idConflict      = "message UUID already reserved by another sender"
)

func admitCases() []admitCase {
	zero := FloodArrival{ID: "zero", Sender: "other"}
	return []admitCase{
		{"too many recent arrivals", func(s *FloodState, now time.Time) { s.Recent = arrivals(now, floodInboundPerHour+1) }, "new", CodeInvalidInput, tooManyArrivals},
		{"an arrival without a time", func(s *FloodState, now time.Time) { s.Recent = []FloodArrival{zero} }, "new", CodeInvalidInput, badArrivalTime},
		{"arrivals out of order", func(s *FloodState, now time.Time) {
			s.Recent = []FloodArrival{{ID: "b", Sender: "other", At: now.Add(-time.Second)}, {ID: "a", Sender: "other", At: now.Add(-2 * time.Second)}}
		}, "new", CodeInvalidInput, badArrivalTime},
		{"an arrival after the inbound bucket", func(s *FloodState, now time.Time) {
			s.Recent = []FloodArrival{{ID: "late", Sender: "other", At: now.Add(time.Second)}}
		}, "new", CodeInvalidInput, badArrivalTime},
		{"a sender bucket without a time", func(s *FloodState, now time.Time) { s.Senders["x"] = FloodBucket{Tokens: 1} }, "new", CodeInvalidInput, badSenderBucket},
		{"a sender bucket below zero", func(s *FloodState, now time.Time) { s.Senders["x"] = FloodBucket{Tokens: -1, At: now} }, "new", CodeInvalidInput, badSenderBucket},
		{"a sender bucket over the burst", func(s *FloodState, now time.Time) {
			s.Senders["x"] = FloodBucket{Tokens: floodPairBurst + 1, At: now}
		}, "new", CodeInvalidInput, badSenderBucket},
		{"a sender bucket that is not a number", func(s *FloodState, now time.Time) {
			s.Senders["x"] = FloodBucket{Tokens: math.NaN(), At: now}
		}, "new", CodeInvalidInput, badSenderBucket},
		// Two faults at once: the earlier check in the list above wins.
		{"too many arrivals and an arrival without a time", func(s *FloodState, now time.Time) {
			s.Recent = append(arrivals(now, floodInboundPerHour), zero)
		}, "new", CodeInvalidInput, tooManyArrivals},
		{"an arrival without a time and a bad sender bucket", func(s *FloodState, now time.Time) {
			s.Recent = []FloodArrival{zero}
			s.Senders["x"] = FloodBucket{Tokens: -1, At: now}
		}, "new", CodeInvalidInput, badArrivalTime},
		{"a time check comes before the age check", func(s *FloodState, now time.Time) {
			s.Recent = []FloodArrival{{ID: "ancient", Sender: "other", At: now.Add(-48 * time.Hour)}, {ID: "b", Sender: "other", At: now.Add(-49 * time.Hour)}}
		}, "new", CodeInvalidInput, badArrivalTime},
		{"the pair hourly cap comes before a bad sender bucket", func(s *FloodState, now time.Time) {
			for i := range floodPairPerHour {
				s.Recent = append(s.Recent, FloodArrival{ID: "p" + time.Duration(i).String(), Sender: "sender", At: now.Add(-time.Duration(floodPairPerHour-i) * time.Second)})
			}
			s.Senders["x"] = FloodBucket{Tokens: -1, At: now}
		}, "new", CodeRateLimited, "pair hourly cap"},
		// An exact retry or a conflict returns before later arrivals are checked.
		{"a retry hides a bad arrival after it", func(s *FloodState, now time.Time) {
			s.Recent = []FloodArrival{{ID: "same", Sender: "sender", At: now.Add(-time.Second)}, zero}
		}, "same", "", ""},
		{"a conflict hides a bad arrival after it", func(s *FloodState, now time.Time) {
			s.Recent = []FloodArrival{{ID: "same", Sender: "other", At: now.Add(-time.Second)}, zero}
		}, "same", CodeConflict, idConflict},
		{"a bad arrival before the retry is still refused", func(s *FloodState, now time.Time) {
			s.Recent = []FloodArrival{zero, {ID: "same", Sender: "sender", At: now.Add(-time.Second)}}
		}, "same", CodeInvalidInput, badArrivalTime},
	}
}

func TestFloodAdmitRefusesInvalidStateInOrder(t *testing.T) {
	for _, test := range admitCases() {
		t.Run(test.name, func(t *testing.T) {
			_, changed, err := admitWith(test.change, "sender", test.id)
			if changed {
				t.Fatal("an invalid or refused state admitted a message")
			}
			assertAdmitError(t, err, test)
		})
	}
}

// assertAdmitError checks the error of one case. An empty wantText means an
// exact retry: no error and nothing admitted.
func assertAdmitError(t *testing.T, err error, test admitCase) {
	t.Helper()
	if test.wantText == "" {
		if err != nil {
			t.Fatalf("exact retry = %v, want no error", err)
		}
		return
	}
	// A rate limit names the wait time, so only its start is fixed.
	text := string(test.wantCode) + ": " + test.wantText
	matches := err != nil && err.Error() == text
	if test.wantCode == CodeRateLimited {
		matches = err != nil && strings.Contains(err.Error(), test.wantText)
	}
	if !matches || ErrorCodeOf(err) != test.wantCode {
		t.Fatalf("error = %v (code %q), want %q (code %q)", err, ErrorCodeOf(err), test.wantText, test.wantCode)
	}
}

func TestFloodAdmitDoesNotDropAnInvalidSenderBucketAsIdle(t *testing.T) {
	state, _, err := admitWith(func(s *FloodState, now time.Time) {
		s.Senders["stale"] = FloodBucket{Tokens: -1, At: now.Add(-time.Hour)}
	}, "sender", "new")
	if err == nil || err.Error() != "invalid_input: "+badSenderBucket {
		t.Fatalf("error = %v", err)
	}
	if _, kept := state.Senders["stale"]; !kept {
		t.Fatal("an invalid bucket was dropped instead of refused")
	}
}
