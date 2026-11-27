package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// plainAdapter implements Adapter only: no hook interface.
type plainAdapter struct{ descriptor agent.AdapterDescriptor }

func (a plainAdapter) Descriptor() agent.AdapterDescriptor { return a.descriptor }
func (plainAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "test", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook}
}
func (plainAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}

// streamingAdapter also opts one event into the streaming payload selector.
type streamingAdapter struct{ keyAdapter }

func (streamingAdapter) ExtendedHookEvent() string { return "PostToolUse" }

func declared(mutate func(*agent.AdapterDescriptor)) agent.AdapterDescriptor {
	d := agent.AdapterDescriptor{Kind: "declared", Label: "Declared", HookEventKey: "hook_event_name", Hooks: map[string]string{"start": "SessionStart"}}
	if mutate != nil {
		mutate(&d)
	}
	return d
}

type declarationCase struct {
	name    string
	adapter agent.Adapter
	lines   []string // each must be a separate line of the error, in this order; none means valid
}

// declarationCases is the table: one case per rule, and cases with several broken rules.
func declarationCases() []declarationCase {
	sentences := func(d *agent.AdapterDescriptor, receiver, acknowledged string) {
		d.UncertainReceiver, d.UncertainAcknowledged = receiver, acknowledged
	}
	return []declarationCase{
		{"valid hook adapter", keyAdapter{descriptor: declared(nil)}, nil},
		{"valid adapter without hooks", plainAdapter{descriptor: agent.AdapterDescriptor{Kind: "plain", Label: "Plain"}}, nil},
		{"valid wording and native fields", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) {
			sentences(d, "in your thread: %s", "ACKed %s")
			d.Native = []agent.NativeField{{Key: "status"}, {Key: "loop_count"}}
		})}, nil},
		{"a hook adapter that maps no event", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.Hooks, d.HookEventKey = nil, "" })},
			[]string{"declared implements HookAdapter but maps no native event"}},
		{"no SessionStart", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.Hooks = map[string]string{"stop": "Stop"} })},
			[]string{"declared maps no native event to SessionStart"}},
		{"no event key", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.HookEventKey = "" })},
			[]string{"declared has hooks but declares no HookEventKey; set it in its descriptor"}},
		{"native fields without HookAdapter", plainAdapter{descriptor: agent.AdapterDescriptor{Kind: "plain", Label: "Plain", Native: []agent.NativeField{{Key: "status"}}}},
			[]string{"plain declares native hook fields but is not a HookAdapter"}},
		{"native fields with the streaming selector", streamingAdapter{keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.Native = []agent.NativeField{{Key: "status"}} })}},
			[]string{"declared declares native hook fields together with the streaming payload selector"}},
		{"streaming selector without native fields", streamingAdapter{keyAdapter{descriptor: declared(nil)}}, nil},
		{"empty native key", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.Native = []agent.NativeField{{Key: ""}} })},
			[]string{`declared: native hook field "" is empty or declared twice`}},
		{"native key twice ignoring case", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.Native = []agent.NativeField{{Key: "status"}, {Key: "STATUS"}} })},
			[]string{`declared: native hook field "STATUS" is empty or declared twice`}},
		{"only the receiver sentence", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { sentences(d, "in your thread: %s", "") })},
			[]string{"declared sets only one of UncertainReceiver and UncertainAcknowledged; UncertainAcknowledged is empty"}},
		{"only the ACKed sentence", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { sentences(d, "", "ACKed %s") })},
			[]string{"declared sets only one of UncertainReceiver and UncertainAcknowledged; UncertainReceiver is empty"}},
		{"sentence without %s", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { sentences(d, "a sentence", "ACKed %s") })},
			[]string{"declared: UncertainReceiver is used as a format string and must have exactly one %s"}},
		{"a lone percent sign", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { sentences(d, "ok %", "ACKed %s") })},
			[]string{"declared: UncertainReceiver is used as a format string"}},
		// Without the "exactly one %s" count, a single other verb or a % next to %s would pass.
		{"a single verb that is not %s", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { sentences(d, "ok %s", "ACKed %d") })},
			[]string{"declared: UncertainAcknowledged is used as a format string"}},
		{"a percent sign next to %s", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { sentences(d, "ok %s", "100% ACKed %s") })},
			[]string{"declared: UncertainAcknowledged is used as a format string"}},
		{"a plain install notice", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.InstallNotice = "Open the settings and trust the hooks" })}, nil},
		{"an install notice with a line break", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.InstallNotice = "line one\nline two" })},
			[]string{"declared declares an InstallNotice that is not one plain line"}},
		{"two sentences with problems", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { sentences(d, "no verb", "two %s %s") })},
			[]string{"declared: UncertainReceiver is used as a format string", "declared: UncertainAcknowledged is used as a format string"}},
		// Two different broken rules give two lines, in the fixed order of the rules.
		{"no SessionStart and no key", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) {
			d.Hooks, d.HookEventKey = map[string]string{"stop": "Stop"}, ""
		})}, []string{"declared maps no native event to SessionStart", "declared has hooks but declares no HookEventKey"}},
		// The hook declaration rules run before the event key rules, so their lines come first.
		{"no SessionStart and an invalid key", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) {
			d.Hooks, d.HookEventKey = map[string]string{"stop": "Stop"}, " padded"
		})}, []string{"declared maps no native event to SessionStart", "declared declares an invalid hook event key"}},
		{"no hooks but an event key", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.Hooks = nil })},
			[]string{"declared implements HookAdapter but maps no native event", "declared declares a hook event key but no hooks"}},
		{"valid prompt reply", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) {
			d.Lifecycle = agent.Lifecycle{PromptMayRegister: true, PromptReply: `{"continue": true}`}
		})}, nil},
		{"may register without a reply", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.Lifecycle.PromptMayRegister = true })}, nil},
		{"a reply without PromptMayRegister", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) { d.Lifecycle.PromptReply = `{}` })},
			[]string{"declared declares a PromptReply but not PromptMayRegister"}},
		{"a reply that is not JSON", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) {
			d.Lifecycle = agent.Lifecycle{PromptMayRegister: true, PromptReply: `{"continue":`}
		})}, []string{"declared: PromptReply must be one JSON object on one line"}},
		{"a reply that is an array", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) {
			d.Lifecycle = agent.Lifecycle{PromptMayRegister: true, PromptReply: `[]`}
		})}, []string{"declared: PromptReply must be one JSON object on one line"}},
		{"a reply that is null", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) {
			d.Lifecycle = agent.Lifecycle{PromptMayRegister: true, PromptReply: `null`}
		})}, []string{"declared: PromptReply must be one JSON object on one line"}},
		{"a reply with a newline", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) {
			d.Lifecycle = agent.Lifecycle{PromptMayRegister: true, PromptReply: "{\n}"}
		})}, []string{"declared: PromptReply must be one JSON object on one line"}},
		{"three rules at once", keyAdapter{descriptor: declared(func(d *agent.AdapterDescriptor) {
			d.HookEventKey = ""
			d.Native = []agent.NativeField{{Key: "a"}, {Key: "A"}}
			sentences(d, "in your thread: %s", "")
		})}, []string{"declared has hooks but declares no HookEventKey", `declared: native hook field "A" is empty or declared twice`, "declared sets only one of UncertainReceiver"}},
	}
}

// checkRefusal checks that the registry refuses the adapter with exactly these lines, or accepts it when there are none.
func checkRefusal(t *testing.T, test declarationCase) {
	t.Helper()
	_, err := agent.NewRegistry(test.adapter)
	if test.lines == nil {
		if err != nil {
			t.Fatalf("registry refused a valid declaration:\n%v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("registry accepted a broken declaration, want lines %q", test.lines)
	}
	got := strings.Split(err.Error(), "\n")
	if len(got) != len(test.lines) {
		t.Fatalf("registry reported %d lines, want %d:\n%v", len(got), len(test.lines), err)
	}
	for i, want := range test.lines {
		if !strings.Contains(got[i], want) {
			t.Fatalf("line %d = %q, want it to contain %q", i+1, got[i], want)
		}
	}
}

// Every declaration rule is enforced by the registry, and the registry lists all
// problems of one adapter, one per line, in a fixed order.
func TestRegistryDeclarationRules(t *testing.T) {
	for _, test := range declarationCases() {
		t.Run(test.name, func(t *testing.T) { checkRefusal(t, test) })
	}
}
