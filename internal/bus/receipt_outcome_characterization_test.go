package bus_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// hookDeliveryAdapter delivers through the recipient's next hook, so a send
// first records pending_next_activity and the transport then reports a wake.
func hookDeliveryAdapter(kind string) *fakeAdapter {
	return &fakeAdapter{kind: kind, caps: model.ReceiveCapabilities{
		Delivery: "fake_hook", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, HookDelivery: true,
	}}
}

// TestTransportOutcomesPersistAndControlAcknowledgement pins transport receipt
// statuses on the wire, refusal versus uncertainty, and acknowledgement rules.
// An exact-ID retry never calls the transport again for any outcome.
func TestTransportOutcomesPersistAndControlAcknowledgement(t *testing.T) {
	tests := []struct {
		name        string
		hookAdapter bool
		status      bus.DeliveryStatus
		wireStatus  string
		code        bus.ErrorCode
		text        string
		// wantCode is the stored code. A receipt that carries an error text
		// but no code is stored as "unavailable".
		wantCode bus.ErrorCode
		// canAck is whether the recipient can acknowledge the stored message.
		canAck bool
	}{
		{name: "submitted", status: bus.StatusSubmitted, wireStatus: "submitted", canAck: true},
		{name: "transport written", status: bus.StatusTransportWritten, wireStatus: "transport_written", canAck: true},
		{name: "not sent", status: bus.StatusNotSent, wireStatus: "not_sent", code: bus.CodeUnavailable, text: "no route", wantCode: "unavailable", canAck: false},
		{name: "command failed", status: bus.StatusCommandFailed, wireStatus: "command_failed", code: bus.CodeUnavailable, text: "exit status 1", wantCode: "unavailable", canAck: false},
		{name: "unknown after timeout", status: bus.StatusUnknownTimeout, wireStatus: "unknown_after_timeout", text: "deadline exceeded", wantCode: "unavailable", canAck: true},
		{name: "unknown after write", status: bus.StatusUnknownWrite, wireStatus: "unknown_after_write", text: "connection reset", wantCode: "unavailable", canAck: true},
		{name: "wake hint uncertain", hookAdapter: true, status: bus.StatusWakeUncertain, wireStatus: "wake_hint_uncertain", text: "wake not confirmed", wantCode: "unavailable", canAck: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var target *fakeAdapter
			if test.hookAdapter {
				target = hookDeliveryAdapter("beta")
			} else {
				target = nativeAdapter("beta")
			}
			target.script(func(context.Context, bus.Message, bus.Session) bus.Receipt {
				receipt := bus.NewReceipt(test.status)
				receipt.Code, receipt.Error, receipt.Output = test.code, test.text, "native output"
				return receipt
			})
			store := newStore(t, nativeAdapter("alpha"), target)
			from, to := register(t, store, "alpha"), register(t, store, "beta")
			svc := bus.Service{Store: store}
			request := bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "hello", MessageID: newUUID(t)}

			sent, err := svc.Send(context.Background(), request)
			if err != nil {
				t.Fatalf("a transport outcome must not become a Send error: %v", err)
			}
			if sent.Status != test.status || sent.Receipt == nil || sent.Receipt.Status != test.status ||
				sent.Receipt.Code != test.wantCode || sent.Receipt.Error != test.text {
				t.Fatalf("returned message = %+v receipt %+v, want status %q code %q error %q", sent, sent.Receipt, test.status, test.wantCode, test.text)
			}
			stored := readJSONFile(t, filepath.Join(store.Dir, "messages", request.MessageID+".json"))
			receipt, _ := stored["receipt"].(map[string]any)
			if stored["status"] != test.wireStatus || receipt["status"] != test.wireStatus {
				t.Fatalf("persisted status = %v / receipt %v, want %q", stored["status"], receipt, test.wireStatus)
			}
			if got, _ := receipt["code"].(string); got != string(test.wantCode) {
				t.Fatalf("persisted receipt code = %q, want %q", got, test.wantCode)
			}
			if got, _ := receipt["error"].(string); got != test.text {
				t.Fatalf("persisted receipt error = %q, want %q", got, test.text)
			}
			if got, _ := receipt["output"].(string); got != "native output" {
				t.Fatalf("persisted receipt output = %q", got)
			}

			// Retrying the same message ID reports the stored outcome and never resubmits.
			calls := target.transportCalls()
			again, err := svc.Send(context.Background(), request)
			if err != nil || again.Status != test.status || target.transportCalls() != calls {
				t.Fatalf("retry: status %q err %v, transport calls %d -> %d", again.Status, err, calls, target.transportCalls())
			}

			// A late, different receipt cannot rewrite a settled outcome.
			if test.status != bus.StatusSubmitted {
				if _, err := store.Finish(request.MessageID, bus.NewReceipt(bus.StatusSubmitted)); bus.ErrorCodeOf(err) != bus.CodeInvalidTransition {
					t.Fatalf("late receipt over %q: error code %q (%v), want %q", test.status, bus.ErrorCodeOf(err), err, bus.CodeInvalidTransition)
				}
				if after := readJSONFile(t, filepath.Join(store.Dir, "messages", request.MessageID+".json")); after["status"] != string(test.status) {
					t.Fatalf("a rejected receipt changed the stored status to %v", after["status"])
				}
			}

			acked, err := store.Ack(to.ID, to.Key, request.MessageID)
			if test.canAck {
				if err != nil || acked.AcknowledgedAt == "" {
					t.Fatalf("recipient cannot acknowledge %q: %v", test.status, err)
				}
				return
			}
			if bus.ErrorCodeOf(err) != bus.CodeInvalidTransition {
				t.Fatalf("acknowledging %q: error code %q (%v), want %q", test.status, bus.ErrorCodeOf(err), err, bus.CodeInvalidTransition)
			}
			// Reply is an acknowledgement too, so it is refused for the same reason.
			if _, err := svc.Reply(context.Background(), bus.ReplyRequest{Actor: to.ID, Capability: to.Key, MessageID: request.MessageID, Body: "ok"}); bus.ErrorCodeOf(err) != bus.CodeInvalidTransition {
				t.Fatalf("replying to %q: error code %q (%v)", test.status, bus.ErrorCodeOf(err), err)
			}
		})
	}
}

// TestApprovalHoldIsAnOutcomeNotAnError pins what a sender sees: a recipient
// waiting on a permission prompt gets the message queued, with no error and no
// transport call, while a retired target refuses the send before any record or
// transport exists. (A target that enters approval between reservation and
// delivery is held by a second check that needs a race to reach; the white-box
// tests in approval_test.go cover it.)
func TestApprovalHoldIsAnOutcomeNotAnError(t *testing.T) {
	target := nativeAdapter("beta")
	store := newStore(t, nativeAdapter("alpha"), target)
	from, to := register(t, store, "alpha"), register(t, store, "beta")
	svc := bus.Service{Store: store}
	send := func() (bus.Message, error) {
		return svc.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "hello"})
	}

	if err := store.Update(to.ID, bus.StateApproval, false); err != nil {
		t.Fatal(err)
	}
	held, err := send()
	if err != nil {
		t.Fatalf("a held message must not be an error: %v", err)
	}
	if !held.Held || held.Status != bus.StatusApprovalBlocked || target.transportCalls() != 0 {
		t.Fatalf("held = %v status %q transport calls %d", held.Held, held.Status, target.transportCalls())
	}
	if held.Receipt == nil || held.Receipt.Error == "" {
		t.Fatalf("a held message explains itself in its receipt: %+v", held.Receipt)
	}

	if err := store.Update(to.ID, bus.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	refused, err := send()
	if bus.ErrorCodeOf(err) != bus.CodeUnavailable {
		t.Fatalf("send to a retired session: error code %q (%v), want %q", bus.ErrorCodeOf(err), err, bus.CodeUnavailable)
	}
	if refused.ID != "" || target.transportCalls() != 0 {
		t.Fatalf("a refused send created %q or reached the transport (%d calls)", refused.ID, target.transportCalls())
	}
}
