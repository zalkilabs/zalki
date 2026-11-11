package bus_test

import (
	"context"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// TestPersistedJournalLayout pins what the current build writes: directory and
// file names, JSON keys and values. Older builds and later package moves must
// preserve this shape during the refactor. Random identifiers, timestamps,
// key hashes and adjustable flood token balances are normalized. JSON key
// order is deliberately ignored; schema versions and remaining values are exact.
func TestPersistedJournalLayout(t *testing.T) {
	send := func(t *testing.T, svc bus.Service, from, to agent) model.Message {
		t.Helper()
		msg, err := svc.Send(context.Background(), bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "hi"})
		if err != nil {
			t.Fatal(err)
		}
		return msg
	}
	tests := []struct {
		name     string
		prefixes []string
		skip     []string
		run      func(t *testing.T, alpha, beta *fakeAdapter, svc bus.Service, from, to agent)
	}{
		{
			name:     "register and send",
			prefixes: []string{"state-formats", "messages", "flood", "sessions/alpha"},
			run: func(t *testing.T, _, _ *fakeAdapter, svc bus.Service, from, to agent) {
				t.Helper()
				send(t, svc, from, to)
			},
		},
		{
			name:     "reply",
			prefixes: []string{"state-formats", "messages"},
			run: func(t *testing.T, _, _ *fakeAdapter, svc bus.Service, from, to agent) {
				t.Helper()
				msg := send(t, svc, from, to)
				if _, err := svc.Reply(context.Background(), bus.ReplyRequest{Actor: to.ID, Capability: to.Key, MessageID: msg.ID, Body: "yo"}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "transport refuses",
			prefixes: []string{"messages"},
			run: func(t *testing.T, _, beta *fakeAdapter, svc bus.Service, from, to agent) {
				t.Helper()
				beta.script(func(context.Context, model.Message, model.Session) model.Receipt {
					receipt := model.NewReceipt(model.StatusNotSent)
					receipt.Code, receipt.Error, receipt.Output = model.CodeUnavailable, "no route", "native output"
					return receipt
				})
				send(t, svc, from, to)
			},
		},
		{
			name:     "held for approval",
			prefixes: []string{"messages", "sessions/beta"},
			skip:     []string{".receive."},
			run: func(t *testing.T, _, _ *fakeAdapter, svc bus.Service, from, to agent) {
				t.Helper()
				if err := svc.Store.Update(to.ID, model.StateApproval, false); err != nil {
					t.Fatal(err)
				}
				send(t, svc, from, to)
			},
		},
		{
			name:     "retired session",
			prefixes: []string{"sessions/beta"},
			skip:     []string{".receive."},
			run: func(t *testing.T, _, _ *fakeAdapter, svc bus.Service, _, to agent) {
				t.Helper()
				if err := svc.Store.Update(to.ID, model.StateRetired, true); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			alpha, beta := nativeAdapter("alpha"), nativeAdapter("beta")
			store := newStore(t, alpha, beta)
			from, to := register(t, store, "alpha"), register(t, store, "beta")
			test.run(t, alpha, beta, bus.Service{Store: store}, from, to)
			got := describeJournal(t, store.Dir, test.prefixes, test.skip...)
			if want := layoutGolden[test.name]; got != want {
				t.Errorf("persisted journal changed.\n--- got ---\n%s--- want ---\n%s", got, want)
			}
		})
	}
}

// layoutGolden holds the normalized journal text for each scenario above.
var layoutGolden = map[string]string{
	"register and send": `dir  commits
dir  events
dir  events/records
dir  flood
dir  messages
dir  open-messages
dir  sessions
file events/meta.json
file events/records/00000000000000000001.json
file events/records/00000000000000000002.json
file flood/beta_<uuid>.json
       .inbound.at = "<time>"
       .inbound.tokens = <number>
       .recent[0].at = "<time>"
       .recent[0].id = "<uuid>"
       .recent[0].sender = "<uuid>"
       .schema = 1
       .senders.<uuid>.at = "<time>"
       .senders.<uuid>.tokens = <number>
       .target = "beta:<uuid>"
file lock
file messages/<uuid>.json
       .body = "hi"
       .created_at = "<time>"
       .from = "alpha:<uuid>"
       .from_label = "alpha"
       .id = "<uuid>"
       .receipt.at = "<time>"
       .receipt.status = "submitted"
       .schema = 1
       .status = "submitted"
       .target_generation = "<uuid>"
       .to = "beta:<uuid>"
file sessions/alpha_<uuid>.json
       .cwd = "/workspace"
       .generation = "<uuid>"
       .id = "alpha:<uuid>"
       .key_hash = "<hash>"
       .kind = "alpha"
       .native_id = "<uuid>"
       .receive.busy_delivery = "next_turn"
       .receive.delivery = "fake_native"
       .receive.experimental = false
       .receive.hook_delivery = false
       .receive.idle_wake = true
       .receive.inbox = "native"
       .receive.lifecycle_hooks = false
       .receive.requires_endpoint = false
       .receive.requires_listener = false
       .retired = false
       .schema = 1
       .state = "ready"
       .updated_at = "<time>"
file sessions/beta_<uuid>.json
file state-formats.json
       .formats.flood = 1
       .formats.messages = 1
       .formats.sessions = 1
       .schema = 1
`,
	"reply": `dir  commits
dir  events
dir  events/records
dir  flood
dir  messages
dir  open-messages
dir  sessions
dir  transactions
file events/meta.json
file events/records/00000000000000000001.json
file events/records/00000000000000000002.json
file flood/alpha_<uuid>.json
file flood/beta_<uuid>.json
file lock
file messages/<uuid>.json
       .acknowledged_at = "<time>"
       .body = "hi"
       .created_at = "<time>"
       .from = "alpha:<uuid>"
       .from_label = "alpha"
       .id = "<uuid>"
       .receipt.at = "<time>"
       .receipt.status = "submitted"
       .schema = 1
       .status = "submitted"
       .target_generation = "<uuid>"
       .to = "beta:<uuid>"
file messages/<uuid>.json
       .body = "yo"
       .created_at = "<time>"
       .from = "beta:<uuid>"
       .from_label = "beta"
       .id = "<uuid>"
       .receipt.at = "<time>"
       .receipt.status = "submitted"
       .reply_to = "<uuid>"
       .schema = 1
       .status = "submitted"
       .target_generation = "<uuid>"
       .to = "alpha:<uuid>"
file sessions/alpha_<uuid>.json
file sessions/beta_<uuid>.json
file state-formats.json
       .formats.flood = 1
       .formats.messages = 1
       .formats.sessions = 1
       .formats.transactions = 1
       .schema = 1
`,
	"transport refuses": `dir  commits
dir  events
dir  events/records
dir  flood
dir  messages
dir  open-messages
dir  sessions
file events/meta.json
file events/records/00000000000000000001.json
file events/records/00000000000000000002.json
file flood/beta_<uuid>.json
file lock
file messages/<uuid>.json
       .body = "hi"
       .created_at = "<time>"
       .from = "alpha:<uuid>"
       .from_label = "alpha"
       .id = "<uuid>"
       .receipt.at = "<time>"
       .receipt.code = "unavailable"
       .receipt.error = "no route"
       .receipt.output = "native output"
       .receipt.status = "not_sent"
       .schema = 1
       .status = "not_sent"
       .target_generation = "<uuid>"
       .to = "beta:<uuid>"
file sessions/alpha_<uuid>.json
file sessions/beta_<uuid>.json
file state-formats.json
`,
	"held for approval": `dir  commits
dir  events
dir  events/records
dir  flood
dir  messages
dir  open-messages
dir  sessions
file events/meta.json
file events/records/00000000000000000001.json
file events/records/00000000000000000002.json
file events/records/00000000000000000003.json
file flood/beta_<uuid>.json
file lock
file messages/<uuid>.json
       .body = "hi"
       .created_at = "<time>"
       .from = "alpha:<uuid>"
       .from_label = "alpha"
       .held = true
       .id = "<uuid>"
       .receipt.at = "<time>"
       .receipt.error = "queued until the permission prompt is answered"
       .receipt.status = "not_sent_pending_approval"
       .schema = 1
       .status = "not_sent_pending_approval"
       .target_generation = "<uuid>"
       .to = "beta:<uuid>"
file open-messages/<uuid>
file sessions/alpha_<uuid>.json
file sessions/beta_<uuid>.json
       .approval_held = 1
       .cwd = "/workspace"
       .generation = "<uuid>"
       .id = "beta:<uuid>"
       .key_hash = "<hash>"
       .kind = "beta"
       .native_id = "<uuid>"
       .retired = false
       .schema = 1
       .state = "approval"
       .updated_at = "<time>"
file state-formats.json
`,
	"retired session": `dir  commits
dir  events
dir  events/records
dir  messages
dir  open-messages
dir  sessions
file events/meta.json
file events/records/00000000000000000001.json
file events/records/00000000000000000002.json
file events/records/00000000000000000003.json
file lock
file sessions/alpha_<uuid>.json
file sessions/beta_<uuid>.json
       .cwd = "/workspace"
       .generation = "<uuid>"
       .id = "beta:<uuid>"
       .key_hash = "<hash>"
       .kind = "beta"
       .native_id = "<uuid>"
       .retired = true
       .retired_reason = "session_end"
       .schema = 1
       .state = "retired"
       .updated_at = "<time>"
file state-formats.json
`,
}
