// Package agenttest checks that an adapter follows the contracts of package
// agent. It is test support and is never imported by product code.
package agenttest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// events are the common lifecycle events a descriptor may map native names onto.
var events = agent.StandardEvents(true)

type reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// startupGolden is the file, relative to the package of the adapter's tests,
// that holds the exact startup output of the adapter for startupFixture. It is
// optional: Run compares the output with it when the file exists. A failing
// run prints the bytes it got, so a new file can be made from that line.
const startupGolden = "testdata/startup-output.jsonl"

// Run checks an adapter's declaration, hooks, startup output and state codec.
// An adapter can pin the bytes of its startup output in testdata/startup-output.jsonl,
// for the fixture context below.
// The declaration is checked by the registry itself (agent.NewRegistry), the
// same code core runs at start-up, so no declaration rule exists only here. The
// other checks are behavior checks: they call the adapter's own code with
// fixtures, which production construction must not do.
func Run(t *testing.T, adapter agent.Adapter) {
	t.Helper()
	t.Run("declaration", func(t *testing.T) { checkDeclaration(t, adapter) })
	t.Run("hooks", func(t *testing.T) { checkHooks(t, adapter) })
	t.Run("startup", func(t *testing.T) { checkStartup(t, adapter, os.DirFS("."), startupGolden) })
	t.Run("state", func(t *testing.T) { checkState(t, adapter) })
}

// startupFixture is the context the startup output check and the golden files use.
// The instructions hold characters that JSON escapes, so a golden file also pins the escaping.
func startupFixture() agent.StartupContext {
	return agent.StartupContext{
		Actor: "claude:00000000-0000-4000-8000-0000000000d4", Capability: "fixture-capability",
		StateDir: "/fixture/state dir", ConfigFile: "/fixture/config.json", Executable: "/fixture/bin/zalki's",
		Instructions: "Zalki actor: A. Fixture <b> & \"quoted\" text, caf\u00e9, line\nbreak, U+2028 \u2028 end.",
	}
}

// checkDeclaration fails with every problem the registry finds, one per line.
func checkDeclaration(t reporter, adapter agent.Adapter) {
	t.Helper()
	if _, err := agent.NewRegistry(adapter); err != nil {
		t.Errorf("the registry refuses this adapter's declaration:\n%v", err)
	}
}

func checkHooks(t reporter, adapter agent.Adapter) {
	t.Helper()
	hooks, ok := adapter.(agent.HookAdapter)
	if !ok {
		return
	}
	for event := range events {
		for _, native := range nativeNames(adapter.Descriptor(), event) {
			checkHookEvent(t, hooks, event, native)
		}
	}
}

// nativeNames lists the native events the descriptor maps to the common event,
// in a fixed order. When it maps none, the common name stands in, so every
// common event is still tried.
func nativeNames(d agent.AdapterDescriptor, event string) []string {
	var names []string
	for native, common := range d.Hooks {
		if common == event {
			names = append(names, native)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		names = []string{event}
	}
	return names
}

// checkHookEvent calls the adapter as core does: Event is the common event and
// NativeEvent is the name the client sent.
func checkHookEvent(t reporter, hooks agent.HookAdapter, event, native string) {
	t.Helper()
	var out agent.HookInput
	var err error
	if message := panicked(func() {
		out, err = hooks.NormalizeHook(agent.HookInput{Event: event, NativeEvent: native, SessionID: "fixture", CWD: "/fixture"})
	}); message != nil {
		t.Errorf("%s (%s): NormalizeHook panicked: %v", event, native, message)
		return
	}
	if err != nil {
		return
	}
	if out.AllowPromptRegistration {
		t.Errorf("%s (%s): NormalizeHook must not allow prompt registration; the descriptor does", event, native)
	}
	if events[out.Event] == "" {
		t.Errorf("%s (%s): NormalizeHook returned the unknown event %q", event, native, out.Event)
	}
	if message := panicked(func() { _, _ = hooks.HandleHook(host{}, out, "fixture", io.Discard) }); message != nil {
		t.Errorf("%s (%s): HandleHook panicked: %v", event, native, message)
	}
}

// checkStartup checks the startup output of a hook adapter. It checks behavior,
// not a format: core must not know which format a client reads, so each adapter
// renders its own. The output must be one JSON object, must carry the
// instructions exactly once, and must be the same for the same input. When a
// golden file exists, the bytes must be exactly those.
func checkStartup(t reporter, adapter agent.Adapter, files fs.FS, golden string) {
	t.Helper()
	hooks, ok := adapter.(agent.HookAdapter)
	if !ok {
		return
	}
	startup := startupFixture()
	first, err := renderStartup(hooks, startup)
	if err != nil {
		t.Errorf("StartupOutput failed for the fixture: %v", err)
		return
	}
	if !startupIsOneObjectWith(first, startup.Instructions) {
		t.Errorf("StartupOutput must be one non-empty JSON object that carries the instructions exactly once, got %q", first)
	}
	if second, err := renderStartup(hooks, startup); err != nil || !bytes.Equal(first, second) {
		t.Errorf("StartupOutput must give the same bytes for the same input, got %q and then %q (error %v)", first, second, err)
	}
	want, err := fs.ReadFile(files, golden)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		t.Errorf("cannot read the golden file %s: %v", golden, err)
	case !bytes.Equal(first, want):
		t.Errorf("StartupOutput differs from %s\ngot:  %q\nwant: %q", golden, first, want)
	}
}

// renderStartup returns what core writes for one startup output: the JSON of
// the adapter's value and a newline. A panic of the adapter becomes an error.
func renderStartup(hooks agent.HookAdapter, startup agent.StartupContext) (raw []byte, err error) {
	if message := panicked(func() {
		var value any
		if value, err = hooks.StartupOutput(startup); err == nil {
			raw, err = json.Marshal(value)
		}
	}); message != nil {
		return nil, errors.New("StartupOutput panicked")
	}
	return append(raw, '\n'), err
}

func startupIsOneObjectWith(raw []byte, instructions string) bool {
	var object map[string]any
	if json.Unmarshal(raw, &object) != nil || len(object) == 0 {
		return false
	}
	var text strings.Builder
	var collect func(any)
	collect = func(value any) {
		switch v := value.(type) {
		case string:
			text.WriteString(v)
			text.WriteByte(0)
		case map[string]any:
			for _, item := range v {
				collect(item)
			}
		case []any:
			for _, item := range v {
				collect(item)
			}
		}
	}
	collect(object)
	return strings.Count(text.String(), instructions) == 1
}

func checkState(t reporter, adapter agent.Adapter) {
	t.Helper()
	codec, ok := adapter.(agent.SessionStateAdapter)
	if !ok {
		return
	}
	first, err := codec.DecodeState(agent.StateInput{})
	if err != nil {
		return
	}
	second, err := codec.DecodeState(agent.StateInput{State: first})
	if err != nil || !sameJSON(first, second) {
		t.Errorf("DecodeState must accept its own output unchanged: %v", err)
	}
}

func sameJSON(a, b json.RawMessage) bool {
	var left, right bytes.Buffer
	return json.Compact(&left, a) == nil && json.Compact(&right, b) == nil && left.String() == right.String()
}

// panicked returns the value a call panicked with, or nil. A bad adapter then
// fails its own check instead of stopping the whole test run.
func panicked(call func()) (message any) {
	defer func() { message = recover() }()
	call()
	return nil
}

// host is a lifecycle host that changes nothing and finds no session.
type host struct{}

func (host) UpdateSession(string, model.SessionState, bool) error { return nil }
func (host) EmitPending(string) (string, error)                   { return "", os.ErrNotExist }
func (host) CompletedNativePrompt(string, string) (string, error) { return "", nil }
func (host) ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error)) error {
	return nil
}
func (host) ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error)) (agent.RecoveryGrant, error) {
	return agent.RecoveryGrant{}, nil
}
