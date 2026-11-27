package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
)

// activityAdapter is a client with no prompt hook: its "activity" native event maps
// to the common Activity event.
// mayRegister is the descriptor's ActivityMayRegister; reactivate is whether its
// handler asks core to revive a retired session on an Activity hook; promptReply,
// when set, is a prompt hook that may register with that declared reply.
type activityAdapter struct {
	stubStartup
	mayRegister, reactivate bool
	handlerErr              error // what the handler of an Activity hook returns
	promptReply             string
}

const activityKind = "activitytest"

func (a activityAdapter) Descriptor() agent.AdapterDescriptor {
	hooks := agent.StandardEvents(false)
	hooks["activity"] = "Activity"
	return agent.AdapterDescriptor{
		Kind: activityKind, Label: "Activity test", HookEventKey: "hook_event_name", Hooks: hooks,
		ProcessNames: []string{"no-such-activity-client"}, OwnerRole: "client", Executable: "activity-test",
		Lifecycle: agent.Lifecycle{ActivityMayRegister: a.mayRegister, PromptMayRegister: a.promptReply != "", PromptReply: a.promptReply},
	}
}

func (activityAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "next_hook", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, LifecycleHooks: true, HookDelivery: true}
}

func (activityAdapter) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusPendingActivity)
}

func (activityAdapter) Health(agent.HealthHost, model.Session) (string, string) {
	return "deferred", ""
}

func (a activityAdapter) HandleHook(host agent.HookHost, event agent.HookInput, _ string, _ io.Writer) (bool, error) {
	if event.Event == "Activity" && a.handlerErr != nil {
		return false, a.handlerErr
	}
	if event.Event != "Activity" || !a.reactivate {
		return false, nil
	}
	return false, host.(agent.LifecycleHost).ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{Transition: agent.TransitionPromptReactivate}, nil
	})
}

func activityService(t *testing.T, adapter activityAdapter) Service {
	t.Helper()
	registry, err := agent.NewRegistry(adapter)
	if err != nil {
		t.Fatal(err)
	}
	return Service{Store: Store{dir: filepath.Join(t.TempDir(), "state"), registry: registry}}
}

func activityHook(t *testing.T, service Service, native, event string) (string, error) {
	t.Helper()
	input, err := json.Marshal(map[string]any{"hook_event_name": event, "session_id": native, "cwd": "/workspace"})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = service.Hook(context.Background(), activityKind, bytes.NewReader(input), &out)
	return out.String(), err
}

func activitySession(t *testing.T, service Service) model.Session {
	t.Helper()
	v, _, err := service.Store.Register(activityKind, newID(), "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// captureStderr runs f and returns what it wrote to os.Stderr.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	f()
	os.Stderr = original
	_ = writer.Close()
	raw, _ := io.ReadAll(reader)
	_ = reader.Close()
	return string(raw)
}

// A chat core has not seen (resumed without a start hook, or its record is gone) is
// registered by its first Activity hook when the descriptor says so, and not otherwise.
func TestActivityRegistersAnUnknownSessionOnlyWhenTheDescriptorAllows(t *testing.T) {
	for _, allowed := range []bool{true, false} {
		service := activityService(t, activityAdapter{mayRegister: allowed})
		out, err := activityHook(t, service, newID(), "activity")
		if err != nil || out != "" {
			t.Fatalf("allowed %v: output %q, error %v; want no output and no error", allowed, out, err)
		}
		sessions, listErr := service.Store.List()
		if listErr != nil {
			t.Fatal(listErr)
		}
		if allowed != (len(sessions) == 1) {
			t.Fatalf("allowed %v: %d sessions", allowed, len(sessions))
		}
		if allowed && (sessions[0].State != model.StateBusy || sessions[0].Retired) {
			t.Fatalf("the registered session = %+v, want busy and not retired", sessions[0])
		}
	}
}

// Only an Activity hook registers. A flag a normalizer sets, or any other event, does not.
func TestOnlyAnActivityOrPromptHookCanRegisterWhenAllowed(t *testing.T) {
	service := activityService(t, activityAdapter{mayRegister: true})
	for _, native := range []string{"PostToolUse", "Stop", "PermissionRequest", "SessionEnd"} {
		if out, err := activityHook(t, service, newID(), native); err != nil || out != "" {
			t.Fatalf("%s: output %q, error %v", native, out, err)
		}
	}
	if sessions, err := service.Store.List(); err != nil || len(sessions) != 0 {
		t.Fatalf("%d sessions registered by events that may not register (%v)", len(sessions), err)
	}
	for _, kind := range []string{"UserPromptSubmit"} {
		if registrationAllowed(agent.Lifecycle{ActivityMayRegister: true}, kind) {
			t.Fatalf("ActivityMayRegister lets %s register", kind)
		}
	}
	if registrationAllowed(agent.Lifecycle{PromptMayRegister: true}, "Activity") || !registrationAllowed(agent.Lifecycle{PromptMayRegister: true}, "UserPromptSubmit") || !registrationAllowed(agent.Lifecycle{ActivityMayRegister: true}, "Activity") {
		t.Fatal("registrationAllowed mixes up the two descriptor flags")
	}
}

// A registration that fails never fails the client's hook: the hook exits cleanly,
// says so on stderr, and creates nothing.
func TestAFailedActivityRegistrationNeverFailsTheHook(t *testing.T) {
	service := activityService(t, activityAdapter{mayRegister: true})
	activitySession(t, service) // creates the state directory
	if err := os.WriteFile(filepath.Join(service.Store.dir, "hook-registration-quota.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out string
	var err error
	stderr := captureStderr(t, func() { out, err = activityHook(t, service, newID(), "activity") })
	if err != nil || out != "" || !strings.Contains(stderr, "Zalki could not safely register this chat. The chat was not blocked.") {
		t.Fatalf("output %q, error %v, stderr %q", out, err, stderr)
	}
	if sessions, listErr := service.Store.List(); listErr != nil || len(sessions) != 1 {
		t.Fatalf("%d sessions (%v), want only the one that existed", len(sessions), listErr)
	}
}

// Without the flag, an Activity hook of an unknown session is stale as before:
// no registration, no output, no error, no stderr line.
func TestAnActivityHookThatMayNotRegisterStaysQuiet(t *testing.T) {
	service := activityService(t, activityAdapter{})
	var out string
	var err error
	if stderr := captureStderr(t, func() { out, err = activityHook(t, service, newID(), "activity") }); err != nil || out != "" || stderr != "" {
		t.Fatalf("output %q, error %v, stderr %q", out, err, stderr)
	}
}

// A retired session (a closed chat that a prune retired) is revived by an Activity
// hook only when the adapter's own handler asks for it; core never does it alone.
func TestActivityReactivatesARetiredSessionOnlyWhenTheAdapterAsks(t *testing.T) {
	for _, reactivate := range []bool{true, false} {
		service := activityService(t, activityAdapter{mayRegister: true, reactivate: reactivate})
		session := activitySession(t, service)
		if err := service.Store.Update(session.ID, model.StateRetired, true); err != nil {
			t.Fatal(err)
		}
		if out, err := activityHook(t, service, session.NativeID, "activity"); err != nil || out != "" {
			t.Fatalf("output %q, error %v", out, err)
		}
		got, err := service.Store.session(session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if reactivate && (got.Retired || got.State != model.StateBusy || got.RetiredReason != "") {
			t.Errorf("the session was not revived: %+v", got)
		}
		if !reactivate && (!got.Retired || got.State != model.StateRetired) {
			t.Errorf("core revived a session the adapter did not ask for: %+v", got)
		}
	}
}

// A recorded owner that has exited is dropped by an Activity hook (the chat runs in a
// new process now); an owner that is alive is kept.
func TestActivityDropsAnExitedOwnerAndKeepsALiveOne(t *testing.T) {
	service := activityService(t, activityAdapter{mayRegister: true})
	gone := activitySession(t, service)
	exitedOwner(t, service.Store, gone)
	live := activitySession(t, service)
	live.Owner = selfOwner(t)
	if err := service.Store.locked(func() error { return service.Store.saveSession(live) }); err != nil {
		t.Fatal(err)
	}
	for _, session := range []model.Session{gone, live} {
		if _, err := activityHook(t, service, session.NativeID, "activity"); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := service.Store.session(gone.ID); err != nil || got.Owner != nil {
		t.Fatalf("the exited owner was kept: %+v (%v)", got.Owner, err)
	}
	if got, err := service.Store.session(live.ID); err != nil || got.Owner == nil || *got.Owner != *live.Owner {
		t.Fatalf("the live owner changed: %+v (%v)", got.Owner, err)
	}
}

// refreshOwnerOn decides when a hook looks for the owner process again: a prompt
// always; an Activity only when the recorded owner is missing or has exited; no
// other event.
func TestRefreshOwnerOnFollowsTheEvent(t *testing.T) {
	service := activityService(t, activityAdapter{mayRegister: true})
	noOwner := activitySession(t, service)
	exited := activitySession(t, service)
	exitedOwner(t, service.Store, exited)
	alive := activitySession(t, service)
	alive.Owner = selfOwner(t)
	if err := service.Store.locked(func() error { return service.Store.saveSession(alive) }); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		session model.Session
		event   string
		want    bool
	}{
		{"prompt, owner alive", alive, "UserPromptSubmit", true},
		{"activity, owner alive", alive, "Activity", false},
		{"activity, owner exited", exited, "Activity", true},
		{"activity, no owner", noOwner, "Activity", true},
		{"tool, owner exited", exited, "PostToolUse", false},
		{"stop, owner exited", exited, "Stop", false},
		{"permission, no owner", noOwner, "PermissionRequest", false},
	} {
		if got := refreshOwnerOn(service.Store, test.session.ID, test.event); got != test.want {
			t.Errorf("%s: refresh = %v, want %v", test.name, got, test.want)
		}
	}
}

// A chat that was already denied by the registration quota gets the Activity
// wording of the limit message, not the prompt wording ("send another message").
func TestAnActivityHookDeniedByTheQuotaSaysWhenItTriesAgain(t *testing.T) {
	service := activityService(t, activityAdapter{mayRegister: true})
	activitySession(t, service) // creates the state directory
	native := newID()
	actor, err := service.Store.sessionID(activityKind, native)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Store.locked(func() error {
		return debitPromptRegistration(service.Store.dir, actor, time.Now().UTC(), fsutil.WriteAtomic)
	}); err != nil {
		t.Fatal(err)
	}
	var out string
	stderr := captureStderr(t, func() { out, err = activityHook(t, service, native, "activity") })
	const want = "Zalki setup is limited right now. It tries again after about 60 seconds.\n"
	if err != nil || out != "" || stderr != want {
		t.Fatalf("output %q, error %v, stderr %q; want %q", out, err, stderr, want)
	}
}

// The reply a descriptor declares is for a prompt hook only: an Activity hook of
// an adapter that declares both writes nothing of its own.
func TestAnActivityHookNeverWritesThePromptReply(t *testing.T) {
	service := activityService(t, activityAdapter{mayRegister: true, promptReply: `{"declared":true}`})
	if out, err := activityHook(t, service, newID(), "activity"); err != nil || out != "" {
		t.Fatalf("activity hook: output %q, error %v; want nothing", out, err)
	}
	if out, err := activityHook(t, service, newID(), "UserPromptSubmit"); err != nil || out != "{\"declared\":true}\n" {
		t.Fatalf("prompt hook: output %q, error %v; want the declared reply", out, err)
	}
}

// The stderr line names the step that failed. A delivery error of the adapter's
// handler is not a registration error, and a state error after it is neither.
func TestAnActivityHookFailureNamesItsStep(t *testing.T) {
	service := activityService(t, activityAdapter{mayRegister: true, handlerErr: errors.New("delivery failed")})
	session := activitySession(t, service)
	var out string
	var err error
	stderr := captureStderr(t, func() { out, err = activityHook(t, service, session.NativeID, "activity") })
	const want = "Zalki could not finish its work for this chat (step: handler). The chat was not blocked."
	if err != nil || out != "" || !strings.HasPrefix(stderr, want) || strings.Contains(stderr, "register") {
		t.Fatalf("output %q, error %v, stderr %q; want a line that names the handler step", out, err, stderr)
	}
}
