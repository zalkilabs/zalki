package antigravity

import (
	"bytes"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// stubHost stands in for core where a real store cannot make the case happen:
// a session that vanishes after core looked at it, and a write that fails. The
// embedded interface is nil on purpose; the adapter must use only these methods.
type stubHost struct {
	agent.LifecycleHost
	pending    string
	pendingErr error
	state      []byte
	failApply  int // the number of the ApplyLifecycle call that fails; 0 = none
	calls      int
	changes    []agent.LifecycleChange
	order      []string // "apply", "claim" and "emit", in the order the adapter called them
	retired    bool     // the view the policies see
	grant      agent.RecoveryGrant
	claimErr   error
	claimed    bool // a claim policy asked for a recovery
}

func (h *stubHost) ClaimRecovery(policy func(model.SessionView) (agent.RecoveryDecision, error)) (agent.RecoveryGrant, error) {
	h.order = append(h.order, "claim")
	if h.claimErr != nil {
		return agent.RecoveryGrant{}, h.claimErr
	}
	decision, err := policy(model.SessionView{AdapterState: h.state, Retired: h.retired})
	h.claimed = decision.Recover
	return h.grant, err
}

func (h *stubHost) EmitPending(string) (string, error) {
	h.order = append(h.order, "emit")
	return h.pending, h.pendingErr
}

func (h *stubHost) ApplyLifecycle(policy func(model.SessionView) (agent.LifecycleChange, error)) error {
	h.calls++
	h.order = append(h.order, "apply")
	if h.calls == h.failApply {
		return errors.New("write failed")
	}
	change, err := policy(model.SessionView{AdapterState: h.state, Retired: h.retired})
	if err != nil {
		return err
	}
	h.changes = append(h.changes, change)
	if change.AdapterState != nil {
		h.state = change.AdapterState
	}
	return nil
}

func hookWith(host *stubHost, event string) (handled bool, out string, err error) {
	return hookWithInput(host, agent.HookInput{Event: event})
}

func hookWithInput(host *stubHost, input agent.HookInput) (handled bool, out string, err error) {
	var output bytes.Buffer
	handled, err = New().HandleHook(host, input, "antigravity:c", &output)
	return handled, output.String(), err
}

// A session can vanish between core's check and the adapter's read. The hook then
// ends quietly, as for any session that is not there.
func TestAHookForASessionThatVanishedEndsQuietly(t *testing.T) {
	for _, event := range []string{"Activity", "Stop"} {
		host := &stubHost{pendingErr: os.ErrNotExist, state: []byte(`{"continues":2}`)}
		handled, out, err := hookWith(host, event)
		if err != nil || out != "" || !handled {
			t.Errorf("%s: handled %v, output %q, error %v; want handled, no output, no error", event, handled, out, err)
		}
	}
}

func TestAHookThatCannotReadPendingMessagesFails(t *testing.T) {
	boom := errors.New("read failed")
	for _, event := range []string{"Activity", "Stop"} {
		host := &stubHost{pendingErr: boom}
		if _, out, err := hookWith(host, event); !errors.Is(err, boom) || out != "" {
			t.Errorf("%s: output %q, error %v; want the read error and no output", event, out, err)
		}
	}
}

// The first step of a Stop reads the count and already marks the session ready, so
// a failure after it never leaves the session busy.
func TestAStopMarksTheSessionReadyBeforeItReadsPendingMessages(t *testing.T) {
	host := &stubHost{pendingErr: errors.New("read failed")}
	_, _, _ = hookWith(host, "Stop")
	if len(host.changes) != 1 || host.changes[0].Transition != agent.TransitionReady {
		t.Fatalf("changes = %+v, want one transition to ready", host.changes)
	}
}

// The count is written before a message is marked as emitted: a failed write never
// loses a message, because the message was not touched.
func TestAFailedCountWriteNeverMarksAMessageAsEmitted(t *testing.T) {
	host := &stubHost{pending: "text", failApply: 1}
	handled, out, err := hookWith(host, "Stop")
	if err == nil || out != "" || !handled {
		t.Fatalf("handled %v, output %q, error %v; want an error and no reply", handled, out, err)
	}
	if !reflect.DeepEqual(host.order, []string{"apply"}) {
		t.Fatalf("calls = %v, want only the failed write: the message must not be emitted", host.order)
	}
}

func TestAContinueIsCountedBeforeItIsSent(t *testing.T) {
	host := &stubHost{pending: "text", state: []byte(`{"continues":2}`)}
	if handled, out, err := hookWith(host, "Stop"); err != nil || !handled || out != `{"decision":"continue","reason":"text"}`+"\n" {
		t.Fatalf("handled %v, output %q, error %v", handled, out, err)
	}
	if !reflect.DeepEqual(host.order, []string{"apply", "emit"}) || string(host.state) != `{"continues":3}` {
		t.Fatalf("calls = %v, state = %s; want the count 3 written, then the message emitted", host.order, host.state)
	}
}

// At the limit nothing is emitted and the count is reset in the same single write.
func TestTheSixthContinueEmitsNothingAndResetsTheCount(t *testing.T) {
	host := &stubHost{pending: "text", state: []byte(`{"continues":5}`)}
	if handled, out, err := hookWith(host, "Stop"); err != nil || !handled || out != "" {
		t.Fatalf("handled %v, output %q, error %v", handled, out, err)
	}
	if !reflect.DeepEqual(host.order, []string{"apply"}) || string(host.state) != `{}` {
		t.Fatalf("calls = %v, state = %s; want one write that resets the count and no emit", host.order, host.state)
	}
}

// A Stop with nothing pending undoes the provisional count in a second write; if
// that write fails, the hook fails and no reply is sent.
func TestAStopWithNothingPendingResetsTheCountAndSendsNothing(t *testing.T) {
	host := &stubHost{state: []byte(`{"continues":2}`)}
	if _, out, err := hookWith(host, "Stop"); err != nil || out != "" || string(host.state) != `{}` {
		t.Fatalf("output %q, error %v, state %s", out, err, host.state)
	}
	failing := &stubHost{failApply: 2}
	if _, out, err := hookWith(failing, "Stop"); err == nil || out != "" {
		t.Fatalf("output %q, error %v; want an error and no reply", out, err)
	}
}

// After a Stop or an interrupt the adapter has done the whole lifecycle itself, so core must not repeat it.
func TestStopAndInterruptAreHandledByTheAdapter(t *testing.T) {
	for event, pending := range map[string]string{"Stop": "text", "Interrupt": "text"} {
		host := &stubHost{pending: pending}
		handled, _, err := hookWith(host, event)
		if err != nil || !handled {
			t.Errorf("%s: handled %v, error %v", event, handled, err)
		}
	}
}

// Before a model call the adapter marks the chat busy itself (one write that also
// revives a retired chat), so core does not repeat it.
func TestABeforeModelCallHookMarksTheChatBusyItself(t *testing.T) {
	for _, pending := range []string{"", "text"} {
		host := &stubHost{pending: pending}
		handled, _, err := hookWith(host, "Activity")
		if err != nil || !handled || len(host.changes) != 1 || host.changes[0].Transition != agent.TransitionPromptReactivate {
			t.Errorf("pending %q: handled %v, error %v, changes %+v; want handled with one reactivating write", pending, handled, err, host.changes)
		}
	}
}

// A retired chat comes back with its continue count reset; a live chat keeps it.
func TestARetiredChatComesBackWithItsCountReset(t *testing.T) {
	retired := &stubHost{retired: true, state: []byte(`{"continues":4}`)}
	if _, _, err := hookWith(retired, "Activity"); err != nil || string(retired.state) != `{}` {
		t.Fatalf("retired: state %s, error %v; want the count reset", retired.state, err)
	}
	live := &stubHost{state: []byte(`{"continues":4}`)}
	if _, _, err := hookWith(live, "Activity"); err != nil || string(live.state) != `{"continues":4}` || live.changes[0].AdapterState != nil {
		t.Fatalf("live: state %s, error %v; want the count kept and no state written", live.state, err)
	}
}

// A chat that core registered at this hook gets credentials, once, and its pending
// messages in the same reply. A chat that existed before gets no claim.
func TestACreatedChatGetsItsCredentialsWithItsMessages(t *testing.T) {
	grant := agent.RecoveryGrant{Instructions: "INSTRUCTIONS", Capability: "cap"}
	host := &stubHost{grant: grant, pending: "MESSAGE"}
	handled, out, err := hookWithInput(host, agent.HookInput{Event: "Activity", Registered: true})
	want := `{"injectSteps":[{"systemMessage":{"systemMessage":"INSTRUCTIONS ` + permissionNote + `\nMESSAGE"}}]}` + "\n"
	if err != nil || !handled || out != want || !host.claimed || !reflect.DeepEqual(host.order, []string{"claim", "apply", "emit"}) {
		t.Fatalf("handled %v, error %v, claimed %v, calls %v, output %q; want %q", handled, err, host.claimed, host.order, out, want)
	}
	existing := &stubHost{grant: grant, pending: "MESSAGE"}
	if _, out, err := hookWithInput(existing, agent.HookInput{Event: "Activity"}); err != nil || existing.claimed || reflect.DeepEqual(existing.order[:1], []string{"claim"}) || out == "" {
		t.Fatalf("existing chat: output %q, error %v, calls %v; want messages only and no claim", out, err, existing.order)
	}
}

// Credentials that core did not issue (a guard refused the claim) send only the messages.
func TestARefusedClaimSendsOnlyTheMessages(t *testing.T) {
	host := &stubHost{pending: "MESSAGE"}
	_, out, err := hookWithInput(host, agent.HookInput{Event: "Activity", Registered: true})
	if err != nil || out != `{"injectSteps":[{"systemMessage":{"systemMessage":"MESSAGE"}}]}`+"\n" {
		t.Fatalf("output %q, error %v", out, err)
	}
}

// Once the credentials are claimed they are sent even when a later step fails.
func TestClaimedCredentialsAreSentWhenALaterStepFails(t *testing.T) {
	grant := agent.RecoveryGrant{Instructions: "INSTRUCTIONS", Capability: "cap"}
	readFailed := errors.New("read failed")
	host := &stubHost{grant: grant, pendingErr: readFailed}
	_, out, err := hookWithInput(host, agent.HookInput{Event: "Activity", Registered: true})
	if !errors.Is(err, readFailed) || out != `{"injectSteps":[{"systemMessage":{"systemMessage":"INSTRUCTIONS `+permissionNote+`"}}]}`+"\n" {
		t.Fatalf("output %q, error %v; want the credentials and the read error", out, err)
	}
	writeFailed := &stubHost{grant: grant, failApply: 1}
	if _, out, err := hookWithInput(writeFailed, agent.HookInput{Event: "Activity", Registered: true}); err == nil || out == "" {
		t.Fatalf("output %q, error %v; want the credentials and the write error", out, err)
	}
}

func TestAFailedClaimSendsNothing(t *testing.T) {
	host := &stubHost{claimErr: errors.New("claim failed"), pending: "MESSAGE"}
	if _, out, err := hookWithInput(host, agent.HookInput{Event: "Activity", Registered: true}); err == nil || out != "" {
		t.Fatalf("output %q, error %v; want an error and no reply", out, err)
	}
	if !reflect.DeepEqual(host.order, []string{"claim"}) {
		t.Fatalf("calls = %v: a failed claim must not touch a message", host.order)
	}
}

func TestOtherEventsAreNotHandled(t *testing.T) {
	for _, event := range []string{"SessionStart", "SessionEnd", "UserPromptSubmit", "PermissionRequest", "PostToolUse"} {
		if handled, out, err := hookWith(&stubHost{pending: "text"}, event); handled || out != "" || err != nil {
			t.Errorf("%s: handled %v, output %q, error %v", event, handled, out, err)
		}
	}
}
