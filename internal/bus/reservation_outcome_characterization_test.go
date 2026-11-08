package bus

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestReservationStagesQuotaHoldAndPersistence(t *testing.T) {
	for _, approval := range []bool{false, true} {
		for _, persist := range []bool{false, true} {
			name := reservationOutcomeName(approval, persist)
			t.Run(name, func(t *testing.T) { checkReservationSuccess(t, approval, persist) })
		}
	}
}

func reservationOutcomeName(approval, persist bool) string {
	name := "ready"
	if approval {
		name = "approval"
	}
	if persist {
		return name + "/persisted"
	}
	return name + "/staged"
}

func prepareNamedReservation(t *testing.T, approval bool) *reservationOrderFixture {
	t.Helper()
	f := newReservationOrderFixture(t)
	f.target.Name = "Inbox"
	if approval {
		f.target.State = model.StateApproval
	}
	saveReservationTarget(t, f)
	f.request.To = "beta:Inbox"
	return f
}

func checkReservationSuccess(t *testing.T, approval, persist bool) {
	t.Helper()
	f := prepareNamedReservation(t, approval)
	started := time.Now()
	message, target, fresh, err := reserveCharacterizedMessage(f, persist)
	ended := time.Now()
	if err != nil || fresh == approval {
		t.Fatalf("fresh=%v error=%v, approval=%v", fresh, err, approval)
	}
	assertReservationFailureTarget(t, target, f.target, false)
	assertReservationIdentity(t, message, f, started, ended)
	if message.RequestedTo != f.request.To || message.FromLabel != "alpha" {
		t.Fatalf("reservation lost resolved address or sender label: %+v", message)
	}
	assertReservationHold(t, message, approval, started, ended)
	assertReservationQuotaSample(t, f, message, started, ended)
	assertReservationPersistence(t, f, message, approval, persist)
	if !reflect.DeepEqual(*f.calls, []string{"alpha.liveness", "beta.liveness", "beta.liveness", "alpha.descriptor"}) {
		t.Fatal("reservation changed callback order", *f.calls)
	}
}

func assertReservationHold(t *testing.T, message model.Message, approval bool, started, ended time.Time) {
	t.Helper()
	if !approval {
		if message.Held || message.Status != model.StatusAttempting || message.Receipt != nil {
			t.Fatal("ready message was held or gained a receipt", message)
		}
		return
	}
	if !message.Held || message.Status != model.StatusApprovalBlocked || message.Receipt == nil {
		t.Fatal("approval message was not held", message)
	}
	if message.Receipt.Status != model.StatusApprovalBlocked || message.Receipt.Error != model.ApprovalQueuedNote || message.Receipt.Code != "" {
		t.Fatal("approval receipt changed", message.Receipt)
	}
	assertReservationTimestamp(t, message.Receipt.At, started, ended)
}

func assertReservationQuotaSample(t *testing.T, f *reservationOrderFixture, message model.Message, started, ended time.Time) {
	t.Helper()
	path, err := f.store.floodPath(f.target.ID)
	if err != nil {
		t.Fatal(err)
	}
	var state floodRecord
	if err := readJSON(path, &state); err != nil {
		t.Fatal(err)
	}
	sender := derivedID("zalki/flood/sender/v1", f.request.Actor)
	pair := state.Senders[sender]
	if state.Inbound.Tokens != 47 || pair.Tokens != 11 || len(state.Senders) != 1 || len(state.Recent) != 1 {
		t.Fatalf("reservation did not charge one inbound/pair admission: %+v", state)
	}
	arrival := state.Recent[0]
	if arrival.ID != message.ID || arrival.Sender != sender || !arrival.At.Equal(state.Inbound.At) || !arrival.At.Equal(pair.At) {
		t.Fatalf("quota did not retain one ID and shared clock sample: %+v", state)
	}
	assertReservationHoldAfterQuota(t, message, arrival.At)
	created, err := time.Parse(time.RFC3339Nano, message.CreatedAt)
	if err != nil || arrival.At.Before(created) || arrival.At.Before(started) || arrival.At.After(ended) {
		t.Fatalf("quota clock sample moved outside message creation/admission: %+v", state)
	}
}

func assertReservationPersistence(t *testing.T, f *reservationOrderFixture, message model.Message, approval, persist bool) {
	t.Helper()
	stored, err := f.store.message(message.ID)
	if !persist {
		if !os.IsNotExist(err) || f.logReads != 0 {
			t.Fatal("staged reservation persisted or logged", stored, err)
		}
	} else {
		if err != nil || !reflect.DeepEqual(stored, message) {
			t.Fatal("persisted reservation differs from returned message", stored, message, err)
		}
		if f.logReads != 1 || f.logReadErr != nil || !reflect.DeepEqual(f.loggedMessage, message) {
			t.Fatal("reservation log did not observe exactly one already-durable message", f.logReads, f.logReadErr)
		}
	}
	wantHeld := 0
	if approval && persist {
		wantHeld = 1
	}
	current, err := f.store.session(f.target.ID)
	if err != nil || current.ApprovalHeld != wantHeld {
		t.Fatalf("held count=%d, want %d: %v", current.ApprovalHeld, wantHeld, err)
	}
}

func TestReservationPersistenceFailureKeepsFreshnessAndQuota(t *testing.T) {
	for _, approval := range []bool{false, true} {
		t.Run(reservationOutcomeName(approval, true), func(t *testing.T) {
			checkReservationPersistenceFailure(t, approval)
		})
	}
}

func checkReservationPersistenceFailure(t *testing.T, approval bool) {
	t.Helper()
	f := prepareNamedReservation(t, approval)
	// A directory at the final file forces reservation IO to fail even as root.
	path := filepath.Join(f.store.Dir, "messages", f.request.MessageID+".json")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	message, target, fresh, err := reserveCharacterizedMessage(f, true)
	ended := time.Now()
	if err == nil || fresh == approval {
		t.Fatalf("write failure fresh=%v error=%v, approval=%v", fresh, err, approval)
	}
	assertReservationFailureTarget(t, target, f.target, false)
	assertReservationIdentity(t, message, f, started, ended)
	assertReservationHold(t, message, approval, started, ended)
	assertReservationQuotaSample(t, f, message, started, ended)
	current, readErr := f.store.session(f.target.ID)
	if readErr != nil || current.ApprovalHeld != 0 || f.logReads != 0 {
		t.Fatal("failed write kept an approval count or logged a reservation", current.ApprovalHeld, f.logReads, readErr)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || !info.IsDir() {
		t.Fatal("failed write replaced its blocked destination", statErr)
	}
}

func assertReservationHoldAfterQuota(t *testing.T, message model.Message, admitted time.Time) {
	t.Helper()
	if !message.Held {
		return
	}
	held, err := time.Parse(time.RFC3339Nano, message.Receipt.At)
	if err != nil || held.Before(admitted) {
		t.Fatal("approval hold timestamp precedes quota admission", message.Receipt.At, admitted, err)
	}
}
