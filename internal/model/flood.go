package model

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"time"
)

// FloodRetryError reports a quota refusal with its rounded wait and UTC retry time.
func FloodRetryError(scope, target string, now, retry time.Time) error {
	wait := retry.Sub(now).Round(time.Second)
	if wait < retry.Sub(now) {
		wait += time.Second
	}
	return NewError(CodeRateLimited, "rate limited: %s to %s; retry in %s (at %s); wait or reply less often", scope, target, wait, retry.UTC().Format(time.RFC3339Nano))
}

const (
	floodPairBurst        = 12
	floodPairPerMinute    = 12
	floodInboundBurst     = 48
	floodInboundPerMinute = 40
	floodPairPerHour      = 120
	floodInboundPerHour   = 400
	floodSenderTTL        = time.Minute
)

// FloodBucket records a token balance and its refill watermark.
type FloodBucket struct {
	Tokens float64   `json:"tokens"`
	At     time.Time `json:"at"`
}

// FloodArrival records one precommit quota reservation within the hourly window.
type FloodArrival struct {
	ID     string    `json:"id"`
	Sender string    `json:"sender"`
	At     time.Time `json:"at"`
}

// FloodState contains quota values without the host's persistence header.
type FloodState struct {
	Inbound FloodBucket            `json:"inbound"`
	Senders map[string]FloodBucket `json:"senders"`
	Recent  []FloodArrival         `json:"recent"`
}

// NewFloodState initializes quota using the supplied clock, without reading it.
func NewFloodState(now time.Time) FloodState {
	return FloodState{Inbound: FloodBucket{Tokens: floodInboundBurst, At: now}, Senders: map[string]FloodBucket{}}
}

// ValidHeader checks the inbound bucket and sender-map presence only.
// Arrival and sender-entry validation retains its ordered place in admission.
func (state FloodState) ValidHeader() bool {
	return state.Inbound.valid(floodInboundBurst) && state.Senders != nil
}

// FloodAdmission is a quota decision for the host to apply under its lock.
// Next is meaningful only when Changed is true. An exact retry has neither
// Changed nor Code. A refusal's Reason is fixed text, or the limiting scope
// when Code is CodeRateLimited; RetryAt then gives the exact retry instant.
type FloodAdmission struct {
	Next    FloodState
	Changed bool
	Code    ErrorCode
	Reason  string
	RetryAt time.Time
}

// DecideFloodAdmission evaluates a header-validated quota without changing it.
// It reads no clock or external state. Successful output owns its map and slice;
// retries and refusals return no candidate. The host retains error formatting,
// persistence and the transaction that reserves quota before message intent.
func DecideFloodAdmission(state FloodState, senderID, messageID string, now time.Time) FloodAdmission {
	if now.Before(state.Inbound.At) {
		now = state.Inbound.At
	}
	if len(state.Recent) > floodInboundPerHour {
		return FloodAdmission{Code: CodeInvalidInput, Reason: "invalid hourly flood quota state"}
	}
	state.Senders = maps.Clone(state.Senders)
	state.Recent = slices.Clone(state.Recent)
	replay, decision, stopped := replayFloodArrivals(state, senderID, messageID, now)
	if stopped {
		return decision
	}
	state.Recent = replay.recent
	if replay.pairCount >= floodPairPerHour {
		return FloodAdmission{Code: CodeRateLimited, Reason: fmt.Sprintf("pair hourly cap (%d/hour)", floodPairPerHour), RetryAt: replay.oldestPair.Add(time.Hour)}
	}
	if len(replay.recent) >= floodInboundPerHour {
		return FloodAdmission{Code: CodeRateLimited, Reason: fmt.Sprintf("inbound hourly cap (%d/hour, all senders)", floodInboundPerHour), RetryAt: replay.recent[0].At.Add(time.Hour)}
	}
	for actor, bucket := range state.Senders {
		if !bucket.valid(floodPairBurst) {
			return FloodAdmission{Code: CodeInvalidInput, Reason: "invalid sender flood quota state"}
		}
		// A minute of inactivity fully refills a pair, bounding the map.
		if now.Sub(bucket.At) >= floodSenderTTL {
			delete(state.Senders, actor)
		}
	}
	return state.reserveTokens(senderID, messageID, now)
}

type floodReplay struct {
	recent     []FloodArrival
	pairCount  int
	oldestPair time.Time
}

// A matching reservation returns before later arrivals are validated.
// This precedence is characterized, not endorsed.
func replayFloodArrivals(state FloodState, senderID, messageID string, now time.Time) (floodReplay, FloodAdmission, bool) {
	replay := floodReplay{recent: state.Recent[:0]}
	var previous time.Time
	for _, arrival := range state.Recent {
		if arrival.At.IsZero() || arrival.At.Before(previous) || arrival.At.After(state.Inbound.At) {
			return replay, FloodAdmission{Code: CodeInvalidInput, Reason: "invalid flood quota arrival time"}, true
		}
		previous = arrival.At
		if now.Sub(arrival.At) >= time.Hour {
			continue
		}
		replay.recent = append(replay.recent, arrival)
		if arrival.ID == messageID {
			if arrival.Sender != senderID {
				return replay, FloodAdmission{Code: CodeConflict, Reason: "message UUID already reserved by another sender"}, true
			}
			return replay, FloodAdmission{}, true
		}
		if arrival.Sender != senderID {
			continue
		}
		replay.pairCount++
		if replay.oldestPair.IsZero() {
			replay.oldestPair = arrival.At
		}
	}
	return replay, FloodAdmission{}, false
}

func (state FloodState) reserveTokens(senderID, messageID string, now time.Time) FloodAdmission {
	pair, ok := state.Senders[senderID]
	if !ok {
		pair = FloodBucket{Tokens: floodPairBurst, At: now}
	}
	pair = pair.refill(now, floodPairBurst, floodPairPerMinute)
	inbound := state.Inbound.refill(now, floodInboundBurst, floodInboundPerMinute)
	if pair.Tokens < 1 {
		return FloodAdmission{Code: CodeRateLimited, Reason: fmt.Sprintf("pair short bucket (%d burst, %d/minute refill)", floodPairBurst, floodPairPerMinute), RetryAt: pair.nextToken(floodPairPerMinute)}
	}
	if inbound.Tokens < 1 {
		return FloodAdmission{Code: CodeRateLimited, Reason: fmt.Sprintf("inbound short bucket (%d burst, %d/minute refill, all senders)", floodInboundBurst, floodInboundPerMinute), RetryAt: inbound.nextToken(floodInboundPerMinute)}
	}
	pair.Tokens--
	inbound.Tokens--
	state.Senders[senderID], state.Inbound = pair, inbound
	state.Recent = append(state.Recent, FloodArrival{ID: messageID, Sender: senderID, At: now})
	return FloodAdmission{Next: state, Changed: true}
}

func (b FloodBucket) valid(burst int) bool {
	return !b.At.IsZero() && !math.IsNaN(b.Tokens) && b.Tokens >= 0 && b.Tokens <= float64(burst)
}

func (b FloodBucket) refill(now time.Time, burst, perMinute int) FloodBucket {
	// A clock rollback cannot grant tokens or move the refill watermark back.
	if now.After(b.At) {
		b.Tokens = math.Min(float64(burst), b.Tokens+now.Sub(b.At).Minutes()*float64(perMinute))
		b.At = now
	}
	return b
}

func (b FloodBucket) nextToken(perMinute int) time.Time {
	return b.At.Add(time.Duration(math.Ceil((1 - b.Tokens) * float64(time.Minute) / float64(perMinute))))
}
