package bus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestFloodRecordJSONFixtures(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 123456789, time.FixedZone("offset", 2*60*60))
	records := []floodRecord{
		{Schema: 1, Target: "target", Inbound: model.FloodBucket{Tokens: 48, At: now}},
		{Schema: 1, Target: "target", Inbound: model.FloodBucket{Tokens: 48, At: now}, Senders: map[string]model.FloodBucket{}, Recent: []model.FloodArrival{}},
		{Schema: 1, Target: "target", Inbound: model.FloodBucket{Tokens: 47.5, At: now}, Senders: map[string]model.FloodBucket{"sender": {Tokens: 11.25, At: now}}, Recent: []model.FloodArrival{{ID: "message", Sender: "sender", At: now}}},
	}
	want := []string{
		`{"schema":1,"target":"target","inbound":{"tokens":48,"at":"2026-11-01T12:00:00.123456789+02:00"},"senders":null,"recent":null}`,
		`{"schema":1,"target":"target","inbound":{"tokens":48,"at":"2026-11-01T12:00:00.123456789+02:00"},"senders":{},"recent":[]}`,
		`{"schema":1,"target":"target","inbound":{"tokens":47.5,"at":"2026-11-01T12:00:00.123456789+02:00"},"senders":{"sender":{"tokens":11.25,"at":"2026-11-01T12:00:00.123456789+02:00"}},"recent":[{"id":"message","sender":"sender","at":"2026-11-01T12:00:00.123456789+02:00"}]}`,
	}
	for i, record := range records {
		data, err := json.Marshal(record)
		if err != nil || string(data) != want[i] {
			t.Fatalf("fixture%d=%s (%v), want%s", i, data, err, want[i])
		}
		var roundtrip floodRecord
		if err := json.Unmarshal(data, &roundtrip); err != nil {
			t.Fatal(err)
		}
		again, err := json.Marshal(roundtrip)
		if err != nil || !bytes.Equal(data, again) {
			t.Fatalf("fixture%d changed on decode/re-encode: %s (%v)", i, again, err)
		}
	}
}

func floodErrorCases(now time.Time, sender, id string) []struct {
	name string
	edit func(*floodRecord)
	at   time.Time
	code ErrorCode
	want string
} {
	return []struct {
		name string
		edit func(*floodRecord)
		at   time.Time
		code ErrorCode
		want string
	}{
		{"pair short fractional", func(s *floodRecord) { s.Senders[sender] = model.FloodBucket{Tokens: 0.5, At: now} }, now, model.CodeRateLimited, "rate limited: pair short bucket (12 burst, 12/minute refill) to %s; retry in 3s (at 2026-11-01T10:00:02.623456789Z); wait or reply less often"},
		{"inbound short fractional", func(s *floodRecord) { s.Inbound.Tokens = 0.5 }, now, model.CodeRateLimited, "rate limited: inbound short bucket (48 burst, 40/minute refill, all senders) to %s; retry in 1s (at 2026-11-01T10:00:00.873456789Z); wait or reply less often"},
		{"pair hourly", func(s *floodRecord) {
			s.Recent = arrivals(now, 120)
			for i := range s.Recent {
				s.Recent[i].Sender = sender
			}
		}, now, model.CodeRateLimited, "rate limited: pair hourly cap (120/hour) to %s; retry in 58m0s (at 2026-11-01T10:58:00.123456789Z); wait or reply less often"},
		{"inbound hourly", func(s *floodRecord) { s.Recent = arrivals(now, 400) }, now, model.CodeRateLimited, "rate limited: inbound hourly cap (400/hour, all senders) to %s; retry in 53m20s (at 2026-11-01T10:53:20.123456789Z); wait or reply less often"},
		{"rollback wait uses actual clock", func(s *floodRecord) { s.Senders[sender] = model.FloodBucket{At: now} }, now.Add(-time.Minute), model.CodeRateLimited, "rate limited: pair short bucket (12 burst, 12/minute refill) to %s; retry in 1m5s (at 2026-11-01T10:00:05.123456789Z); wait or reply less often"},
		{"reserved conflict", func(s *floodRecord) { s.Recent = []model.FloodArrival{{ID: id, Sender: "other", At: now}} }, now, model.CodeConflict, "message UUID already reserved by another sender"},
		{"invalid arrival", func(s *floodRecord) { s.Recent = []model.FloodArrival{{}} }, now, model.CodeInvalidInput, "invalid flood quota arrival time"},
		{"invalid sender", func(s *floodRecord) { s.Senders["bad"] = model.FloodBucket{Tokens: -1, At: now} }, now, model.CodeInvalidInput, "invalid sender flood quota state"},
	}
}

func TestFloodAdmissionHostErrorsKeepExactTextAndRetryTimes(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 123456789, time.FixedZone("offset", 2*60*60))
	msg := floodMessage()
	for _, test := range floodErrorCases(now, derivedID("zalki/flood/sender/v1", msg.From), msg.ID) {
		t.Run(test.name, func(t *testing.T) {
			store := fixtureStore(t)
			state := withFloodQuota(floodRecord{Schema: 1, Target: msg.To}, model.NewFloodState(now))
			test.edit(&state)
			path, err := store.floodPath(msg.To)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.saveFloodState(path, state); err != nil {
				t.Fatal(err)
			}
			before := floodBytes(t, store, msg.To)
			err = spendFlood(store, msg, test.at)
			want := string(test.code) + ": " + test.want
			if test.code == model.CodeRateLimited {
				want = fmt.Sprintf(want, msg.To)
			}
			if err == nil || ErrorCodeOf(err) != test.code || err.Error() != want {
				t.Fatalf("error=%v, want%s", err, want)
			}
			if !bytes.Equal(before, floodBytes(t, store, msg.To)) {
				t.Fatal("refusal rewrote quota")
			}
			if _, err := os.Stat(store.Dir + "/messages/" + msg.ID + ".json"); !os.IsNotExist(err) {
				t.Fatal("refusal reserved a message", err)
			}
		})
	}
}

func TestFloodHeaderChecksPrecedeRetry(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name string
		edit func(*floodRecord)
		want string
	}{
		{"schema before header", func(s *floodRecord) { s.Schema = 99; s.Target = "other"; s.Senders = nil }, "unsupported_schema: unsupported flood quota schema 99"},
		{"target before retry", func(s *floodRecord) { s.Target = "other" }, "invalid_input: invalid flood quota state"},
		{"inbound before retry", func(s *floodRecord) { s.Inbound.At = time.Time{} }, "invalid_input: invalid flood quota state"},
		{"nil map before retry", func(s *floodRecord) { s.Senders = nil }, "invalid_input: invalid flood quota state"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := fixtureStore(t)
			msg := floodMessage()
			state := withFloodQuota(floodRecord{Schema: 1, Target: msg.To}, model.NewFloodState(now))
			state.Recent = []model.FloodArrival{{ID: msg.ID, Sender: derivedID("zalki/flood/sender/v1", msg.From), At: now}}
			test.edit(&state)
			path, err := store.floodPath(msg.To)
			if err != nil {
				t.Fatal(err)
			}
			// Plant invalid input directly; the production writer rejects future schemas.
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := privateDir(store.Dir + "/flood"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			before := floodBytes(t, store, msg.To)
			if err := spendFlood(store, msg, now); err == nil || err.Error() != test.want {
				t.Fatalf("error=%v,want%s", err, test.want)
			}
			if !bytes.Equal(before, floodBytes(t, store, msg.To)) {
				t.Fatal("invalid header changed")
			}
		})
	}
}
