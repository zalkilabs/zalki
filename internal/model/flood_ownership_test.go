package model

import (
	"maps"
	"math"
	"reflect"
	"slices"
	"testing"
	"time"
)

func floodOwnershipCases() []struct {
	name    string
	edit    func(*FloodState, time.Time)
	code    ErrorCode
	changed bool
} {
	return []struct {
		name    string
		edit    func(*FloodState, time.Time)
		code    ErrorCode
		changed bool
	}{
		{"success compacts expired prefix", func(*FloodState, time.Time) {}, "", true},
		{"retry after expired prefix", func(s *FloodState, _ time.Time) { s.Recent[1].ID = "request" }, "", false},
		{"conflict after expired prefix", func(s *FloodState, _ time.Time) { s.Recent[1].ID, s.Recent[1].Sender = "request", "other" }, CodeConflict, false},
		{"bad arrival after compaction", func(s *FloodState, _ time.Time) { s.Recent = append(s.Recent, FloodArrival{}) }, CodeInvalidInput, false},
		{"pair hourly", func(s *FloodState, now time.Time) { s.Recent = ownershipHistory(now, 120, "sender") }, CodeRateLimited, false},
		{"inbound hourly", func(s *FloodState, now time.Time) { s.Recent = ownershipHistory(now, 400, "other") }, CodeRateLimited, false},
		{"invalid sender", func(s *FloodState, now time.Time) {
			s.Senders["invalid-idle"] = FloodBucket{Tokens: -1, At: now.Add(-time.Hour)}
		}, CodeInvalidInput, false},
		{"pair short after idle deletion", func(s *FloodState, now time.Time) { s.Senders["sender"] = FloodBucket{At: now} }, CodeRateLimited, false},
		{"inbound short after idle deletion", func(s *FloodState, _ time.Time) { s.Inbound.Tokens = 0 }, CodeRateLimited, false},
		{"nil recent", func(s *FloodState, _ time.Time) { s.Recent = nil }, "", true},
		{"empty recent", func(s *FloodState, _ time.Time) { s.Recent = []FloodArrival{} }, "", true},
	}
}

func ownershipHistory(now time.Time, count int, sender string) []FloodArrival {
	list := arrivals(now, count)
	for i := range list {
		list[i].Sender = sender
	}
	return list
}

func ownershipInput(now time.Time) FloodState {
	state := NewFloodState(now)
	state.Senders["idle"] = FloodBucket{At: now.Add(-time.Minute)}
	storage := make([]FloodArrival, 6)
	for i := range storage {
		storage[i] = FloodArrival{ID: "spare sentinel", Sender: "untouched", At: now}
	}
	storage[0] = FloodArrival{ID: "expired", Sender: "sender", At: now.Add(-time.Hour)}
	storage[1] = FloodArrival{ID: "retained", Sender: "sender", At: now.Add(-time.Second)}
	state.Recent = storage[:2]
	return state
}

func TestFloodAdmissionDoesNotMutateInputOrShareSuccessfulStorage(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, test := range floodOwnershipCases() {
		t.Run(test.name, func(t *testing.T) {
			input := ownershipInput(now)
			test.edit(&input, now)
			before := FloodState{Inbound: input.Inbound, Senders: maps.Clone(input.Senders), Recent: slices.Clone(input.Recent)}
			backing := slices.Clone(input.Recent[:cap(input.Recent)])
			got := DecideFloodAdmission(input, "sender", "request", now)
			if got.Code != test.code || got.Changed != test.changed {
				t.Fatalf("decision=%+v, want code=%s changed=%v", got, test.code, test.changed)
			}
			if again := DecideFloodAdmission(input, "sender", "request", now); !reflect.DeepEqual(got, again) {
				t.Fatal("identical facts changed the decision")
			}
			requireFloodInputUnchanged(t, input, before, backing)
			if got.Changed {
				delete(got.Next.Senders, "sender")
				got.Next.Senders["changed"] = FloodBucket{Tokens: 1, At: now}
				got.Next.Recent[0].ID = "changed"
				got.Next.Recent = append(got.Next.Recent, FloodArrival{ID: "appended"})
			} else if !reflect.DeepEqual(got.Next, FloodState{}) {
				t.Fatal("retry/refusal returned a candidate to apply")
			}
			requireFloodInputUnchanged(t, input, before, backing)
		})
	}
}

func requireFloodInputUnchanged(t *testing.T, input, before FloodState, backing []FloodArrival) {
	t.Helper()
	if !reflect.DeepEqual(input, before) {
		t.Fatalf("input changed: before=%+v after=%+v", before, input)
	}
	if !reflect.DeepEqual(input.Recent[:cap(input.Recent)], backing) {
		t.Fatal("input backing storage changed, including spare capacity")
	}
}

func TestFloodHeaderChecksOnlyInboundAndMapPresence(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		edit func(*FloodState)
		want bool
	}{
		{"new", func(*FloodState) {}, true},
		{"nil map", func(s *FloodState) { s.Senders = nil }, false},
		{"zero time", func(s *FloodState) { s.Inbound.At = time.Time{} }, false},
		{"negative", func(s *FloodState) { s.Inbound.Tokens = -1 }, false},
		{"over burst", func(s *FloodState) { s.Inbound.Tokens = 49 }, false},
		{"NaN", func(s *FloodState) { s.Inbound.Tokens = math.NaN() }, false},
		{"infinity", func(s *FloodState) { s.Inbound.Tokens = math.Inf(1) }, false},
		{"empty balance", func(s *FloodState) { s.Inbound.Tokens = 0 }, true},
		{"defer arrival validation", func(s *FloodState) { s.Recent = []FloodArrival{{}} }, true},
		{"defer sender validation", func(s *FloodState) { s.Senders["bad"] = FloodBucket{} }, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := NewFloodState(now)
			test.edit(&state)
			if state.ValidHeader() != test.want {
				t.Fatalf("header validity=%v, want %v", state.ValidHeader(), test.want)
			}
		})
	}
}
