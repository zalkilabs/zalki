package bus

import (
	"maps"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// These cases characterize existing failure priority and partial results.
// They do not endorse the ordering or expand reservation's caller contract.
type reservationFailureCase struct {
	name       string
	prepare    func(*testing.T, *reservationOrderFixture)
	want       string
	code       model.ErrorCode
	status     model.DeliveryStatus
	calls      []string
	zeroTarget bool
}

func reservationFailureCases() []reservationFailureCase {
	live := []string{"beta.liveness"}
	built := []string{"beta.liveness", "alpha.descriptor"}
	return []reservationFailureCase{
		{"destination before sender and quota", reservationBadDestination, "invalid_input: destination must be a complete kind:UUID or an exact native session name", model.CodeInvalidInput, "", nil, true},
		{"retired before exited self and reply", reservationRetiredTarget, "unavailable: destination has retired", model.CodeUnavailable, "", nil, false},
		{"exited before self and reply", reservationExitedTarget, "unavailable: destination owner process has exited", model.CodeUnavailable, "", live, false},
		{"self before reply and sender", reservationSelfTarget, "invalid_input: self messages are not supported", model.CodeInvalidInput, "", live, false},
		{"reply identity before sender capacity and quota", reservationBadReplyID, "invalid_identity: expected an exact session/message UUID", model.CodeInvalidIdentity, "", live, false},
		{"reply recipient before sender capacity and quota", reservationWrongReplyTo, "unauthorized: reply must return to the original sender", model.CodeUnauthorized, "", live, false},
		{"reply sender before sender capacity and quota", reservationWrongReplyFrom, "unauthorized: reply must return to the original sender", model.CodeUnauthorized, "", live, false},
		{"sender before capacity and quota", reservationMissingSender, `invalid_input: unsupported agent "missing"`, model.CodeInvalidInput, "", live, false},
		{"capacity before malformed quota", reservationFullApproval, "unavailable: too many messages are waiting for this session's permission prompt", model.CodeUnavailable, model.StatusAttempting, built, false},
		{"quota failure settles instead of holding", reservationMalformedQuota, "unsupported_schema: unsupported flood quota schema 99", model.CodeUnsupportedSchema, model.StatusNotSent, built, false},
		{"rate refusal settles instead of holding", reservationExhaustedQuota, "", model.CodeRateLimited, model.StatusNotSent, built, false},
	}
}

func TestReservationFailureOrderAndPartialValues(t *testing.T) {
	for _, test := range reservationFailureCases() {
		t.Run(test.name, func(t *testing.T) { checkReservationFailure(t, test) })
	}
}

func checkReservationFailure(t *testing.T, test reservationFailureCase) {
	t.Helper()
	f := newReservationOrderFixture(t)
	test.prepare(t, f)
	before := reservationSnapshot(t, f.store.Dir)
	started := time.Now()
	message, target, fresh, err := reserveCharacterizedMessage(f, true)
	ended := time.Now()
	assertReservationError(t, err, test)
	if fresh {
		t.Fatal("refusal reported a fresh delivery")
	}
	assertReservationFailureTarget(t, target, f.target, test.zeroTarget)
	assertReservationFailureMessage(t, message, err, test, f, started, ended)
	if !reflect.DeepEqual(*f.calls, test.calls) {
		t.Fatalf("callbacks=%v, want %v", *f.calls, test.calls)
	}
	if !maps.Equal(before, reservationSnapshot(t, f.store.Dir)) || f.logReads != 0 {
		t.Fatal("refusal changed journal bytes or logged a reservation")
	}
	if _, err := f.store.message(f.request.MessageID); !os.IsNotExist(err) {
		t.Fatal("refusal persisted an outgoing message", err)
	}
}

func assertReservationError(t *testing.T, err error, test reservationFailureCase) {
	t.Helper()
	if err == nil || model.ErrorCodeOf(err) != test.code {
		t.Fatalf("error=%v, want code %s", err, test.code)
	}
	if test.want != "" && err.Error() != test.want {
		t.Fatalf("error=%v, want %q", err, test.want)
	}
}

func assertReservationFailureTarget(t *testing.T, got, want model.Session, zero bool) {
	t.Helper()
	if zero {
		if !reflect.DeepEqual(got, model.Session{}) {
			t.Fatalf("unresolved target=%+v, want zero value", got)
		}
		return
	}
	if got.ID != want.ID || got.Generation != want.Generation || got.State != want.State || got.Retired != want.Retired {
		t.Fatalf("partial target=%+v, want resolved identity/state %+v", got, want)
	}
}

func assertReservationFailureMessage(t *testing.T, message model.Message, err error, test reservationFailureCase, f *reservationOrderFixture, started, ended time.Time) {
	t.Helper()
	if test.status == "" {
		if !reflect.DeepEqual(message, model.Message{}) {
			t.Fatalf("preflight returned a partial message: %+v", message)
		}
		return
	}
	assertReservationIdentity(t, message, f, started, ended)
	if message.Status != test.status || message.Held {
		t.Fatalf("failed message=%+v, want status %s without hold", message, test.status)
	}
	if test.status == model.StatusAttempting {
		if message.Receipt != nil {
			t.Fatal("capacity refusal created a receipt", message.Receipt)
		}
		return
	}
	if message.Receipt == nil || message.Receipt.Status != model.StatusNotSent || message.Receipt.Code != test.code || message.Receipt.Error != err.Error() {
		t.Fatal("quota failure did not preserve its exact final receipt", message.Receipt, err)
	}
	assertReservationTimestamp(t, message.Receipt.At, started, ended)
}

func assertReservationIdentity(t *testing.T, message model.Message, f *reservationOrderFixture, started, ended time.Time) {
	t.Helper()
	if message.ID != f.request.MessageID || message.From != f.request.Actor || message.To != f.target.ID || message.Body != f.request.Body || message.ReplyTo != f.request.ReplyTo || message.TargetGeneration != f.target.Generation {
		t.Fatalf("reservation identity/body changed: %+v", message)
	}
	assertReservationTimestamp(t, message.CreatedAt, started, ended)
}

func assertReservationTimestamp(t *testing.T, raw string, started, ended time.Time) {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || at.Before(started) || at.After(ended) {
		t.Fatalf("timestamp=%q is outside the reservation call", raw)
	}
}

func reservationBadDestination(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	f.request.To, f.sender.Kind = "", "missing"
	seedInvalidReservationQuota(t, f)
}

func reservationSelfTarget(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	f.request.Actor, f.request.ReplyTo = f.target.ID, "bad"
	f.sender = f.target
	f.sender.Kind = "missing"
}

func reservationRetiredTarget(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	f.target.Retired, f.target.State = true, model.StateRetired
	saveReservationTarget(t, f)
	*f.live = model.LivenessExited
	reservationSelfTarget(t, f)
}

func reservationExitedTarget(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	*f.live = model.LivenessExited
	reservationSelfTarget(t, f)
}

func reservationBadReplyID(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	reservationMissingSender(t, f)
	f.request.ReplyTo = "bad"
}

func reservationWrongReplyTo(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	reservationMissingSender(t, f)
	seedReservationReply(t, f, true)
}

func reservationWrongReplyFrom(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	reservationMissingSender(t, f)
	seedReservationReply(t, f, false)
}

func reservationMissingSender(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	setFullReservationApproval(t, f)
	seedInvalidReservationQuota(t, f)
	f.sender.Kind = "missing"
}

func reservationFullApproval(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	setFullReservationApproval(t, f)
	seedInvalidReservationQuota(t, f)
}

func reservationMalformedQuota(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	f.target.State = model.StateApproval
	saveReservationTarget(t, f)
	seedInvalidReservationQuota(t, f)
}

func reservationExhaustedQuota(t *testing.T, f *reservationOrderFixture) {
	t.Helper()
	f.target.State = model.StateApproval
	saveReservationTarget(t, f)
	seedEmptyReservationQuota(t, f)
}
