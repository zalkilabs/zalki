package bus

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	floodPairBurst        = 12
	floodPairPerMinute    = 12
	floodInboundBurst     = 48
	floodInboundPerMinute = 40
	floodPairPerHour      = 120
	floodInboundPerHour   = 400
	floodSenderTTL        = time.Minute
)

type floodBucket struct {
	Tokens float64   `json:"tokens"`
	At     time.Time `json:"at"`
}

type floodState struct {
	Schema  int                    `json:"schema"`
	Target  string                 `json:"target"`
	Inbound floodBucket            `json:"inbound"`
	Senders map[string]floodBucket `json:"senders"`
	Recent  []floodArrival         `json:"recent"`
}

type floodArrival struct {
	ID     string    `json:"id"`
	Sender string    `json:"sender"`
	At     time.Time `json:"at"`
}

func (b floodBucket) valid(burst int) bool {
	return !b.At.IsZero() && !math.IsNaN(b.Tokens) && b.Tokens >= 0 && b.Tokens <= float64(burst)
}

func (b floodBucket) refill(now time.Time, burst, perMinute int) floodBucket {
	// A clock rollback cannot grant tokens or move the refill watermark back.
	if now.After(b.At) {
		b.Tokens = math.Min(float64(burst), b.Tokens+now.Sub(b.At).Minutes()*float64(perMinute))
		b.At = now
	}
	return b
}

func (s Store) floodPath(target string) (string, error) {
	if _, err := s.sessionPath(target); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, "flood", strings.ReplaceAll(target, ":", "_")+".json"), nil
}

func floodWait(scope, target string, now, retry time.Time) error {
	wait := retry.Sub(now).Round(time.Second)
	if wait < retry.Sub(now) {
		wait += time.Second
	}
	return fail(CodeRateLimited, "rate limited: %s to %s; retry in %s (at %s); wait or reply less often", scope, target, wait, retry.UTC().Format(time.RFC3339Nano))
}

func (b floodBucket) nextToken(perMinute int) time.Time {
	return b.At.Add(time.Duration(math.Ceil((1 - b.Tokens) * float64(time.Minute) / float64(perMinute))))
}

// consumeFloodQuota is called under the journal lock, after the exact-ID retry
// check and validation. One atomic target file contains both buckets, so a
// rejected inbound check never spends a sender token (or vice versa).
func (s Store) consumeFloodQuota(msg Message, now time.Time) error {
	if msg.Kind == noticeKind {
		return nil
	}
	path, err := s.floodPath(msg.To)
	if err != nil {
		return err
	}
	var state floodState
	if err := readJSON(path, &state); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		state = floodState{Schema: 1, Target: msg.To, Inbound: floodBucket{Tokens: floodInboundBurst, At: now}, Senders: map[string]floodBucket{}}
	}
	if state.Schema != 1 {
		return fail(CodeUnsupportedSchema, "unsupported flood quota schema %d", state.Schema)
	}
	if state.Target != msg.To || !state.Inbound.valid(floodInboundBurst) || state.Senders == nil {
		return fail(CodeInvalidInput, "invalid flood quota state")
	}
	actualNow := now
	if now.Before(state.Inbound.At) {
		now = state.Inbound.At
	}
	if len(state.Recent) > floodInboundPerHour {
		return fail(CodeInvalidInput, "invalid hourly flood quota state")
	}
	senderID := derivedID("zalki/flood/sender/v1", msg.From)
	recent := state.Recent[:0]
	pairCount := 0
	var oldestPair time.Time
	var previous time.Time
	for _, arrival := range state.Recent {
		if arrival.At.IsZero() || arrival.At.Before(previous) || arrival.At.After(state.Inbound.At) {
			return fail(CodeInvalidInput, "invalid flood quota arrival time")
		}
		previous = arrival.At
		if now.Sub(arrival.At) >= time.Hour {
			continue
		}
		recent = append(recent, arrival)
		if arrival.ID == msg.ID {
			if arrival.Sender != senderID {
				return fail(CodeConflict, "message UUID already reserved by another sender")
			}
			// A quota reservation can outlive a crash before journal/intent IO.
			// Reusing it must not spend a second token or hourly slot.
			return nil
		}
		if arrival.Sender == senderID {
			pairCount++
			if oldestPair.IsZero() {
				oldestPair = arrival.At
			}
		}
	}
	state.Recent = recent
	if pairCount >= floodPairPerHour {
		return floodWait(fmt.Sprintf("pair hourly cap (%d/hour)", floodPairPerHour), msg.To, actualNow, oldestPair.Add(time.Hour))
	}
	if len(recent) >= floodInboundPerHour {
		return floodWait(fmt.Sprintf("inbound hourly cap (%d/hour, all senders)", floodInboundPerHour), msg.To, actualNow, recent[0].At.Add(time.Hour))
	}
	for actor, bucket := range state.Senders {
		if !bucket.valid(floodPairBurst) {
			return fail(CodeInvalidInput, "invalid sender flood quota state")
		}
		// A minute of inactivity fully refills a pair. Pruning here bounds the
		// map by recent traffic, rather than every historical sender.
		if now.Sub(bucket.At) >= floodSenderTTL {
			delete(state.Senders, actor)
		}
	}
	pair, ok := state.Senders[senderID]
	if !ok {
		pair = floodBucket{Tokens: floodPairBurst, At: now}
	}
	pair = pair.refill(now, floodPairBurst, floodPairPerMinute)
	inbound := state.Inbound.refill(now, floodInboundBurst, floodInboundPerMinute)
	if pair.Tokens < 1 {
		return floodWait(fmt.Sprintf("pair short bucket (%d burst, %d/minute refill)", floodPairBurst, floodPairPerMinute), msg.To, actualNow, pair.nextToken(floodPairPerMinute))
	}
	if inbound.Tokens < 1 {
		return floodWait(fmt.Sprintf("inbound short bucket (%d burst, %d/minute refill, all senders)", floodInboundBurst, floodInboundPerMinute), msg.To, actualNow, inbound.nextToken(floodInboundPerMinute))
	}
	pair.Tokens--
	inbound.Tokens--
	state.Senders[senderID], state.Inbound = pair, inbound
	state.Recent = append(state.Recent, floodArrival{ID: msg.ID, Sender: senderID, At: now})
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	// Reserve quota first: an IO failure or crash may consume capacity without
	// sending, but must not permit uncounted delivery after restarting.
	return writeJSON(path, state)
}
