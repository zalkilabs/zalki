package agenttest

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type recorder struct{ failures []string }

func (r *recorder) Helper() {}
func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// brokenAdapter breaks one rule at a time so each check proves it can fail.
type brokenAdapter struct {
	agent.StandardHooks
	descriptor agent.AdapterDescriptor
	normalize  func(agent.HookInput) (agent.HookInput, error)
	handle     func() (bool, error)
	startup    func(agent.StartupContext) (any, error)
	caps       model.ReceiveCapabilities
}

func (a brokenAdapter) Descriptor() agent.AdapterDescriptor { return a.descriptor }
func (a brokenAdapter) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return a.caps
}
func (brokenAdapter) Deliver(_ context.Context, _ model.Message, _ model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}
func (a brokenAdapter) NormalizeHook(v agent.HookInput) (agent.HookInput, error) {
	if a.normalize != nil {
		return a.normalize(v)
	}
	return v, nil
}
func (a brokenAdapter) HandleHook(agent.HookHost, agent.HookInput, string, io.Writer) (bool, error) {
	if a.handle != nil {
		return a.handle()
	}
	return false, nil
}

// StartupOutput renders a plain test format unless a test replaces it.
func (a brokenAdapter) StartupOutput(v agent.StartupContext) (any, error) {
	if a.startup != nil {
		return a.startup(v)
	}
	return map[string]string{"fixture_context": v.Instructions}, nil
}

func good() brokenAdapter {
	return brokenAdapter{
		descriptor: agent.AdapterDescriptor{Kind: "fixture", Label: "Fixture", HookEventKey: "hook_event_name", Hooks: agent.StandardEvents(true)},
		caps:       model.ReceiveCapabilities{Delivery: "fixture", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook, LifecycleHooks: true},
	}
}

func TestChecksCatchBrokenAdapters(t *testing.T) {
	for _, test := range []struct {
		name   string
		break_ func(*brokenAdapter)
		want   string
	}{
		{"normalizer allows registration", func(a *brokenAdapter) {
			a.normalize = func(v agent.HookInput) (agent.HookInput, error) { v.AllowPromptRegistration = true; return v, nil }
		}, "descriptor does"},
		{"normalizer invents an event", func(a *brokenAdapter) {
			a.normalize = func(v agent.HookInput) (agent.HookInput, error) { v.Event = "Other"; return v, nil }
		}, "unknown event"},
		{"normalizer panics", func(a *brokenAdapter) {
			a.normalize = func(agent.HookInput) (agent.HookInput, error) { panic("boom") }
		}, "NormalizeHook panicked"},
		{"handler panics", func(a *brokenAdapter) { a.handle = func() (bool, error) { panic("boom") } }, "HandleHook panicked"},
	} {
		adapter := good()
		test.break_(&adapter)
		r := &recorder{}
		checkHooks(r, adapter)
		if !strings.Contains(strings.Join(r.failures, "\n"), test.want) {
			t.Fatal(test.name, "was not caught:", r.failures)
		}
	}
	r := &recorder{}
	checkDeclaration(r, good())
	checkHooks(r, good())
	checkStartup(r, good(), os.DirFS(t.TempDir()), "no-golden")
	checkState(r, good())
	if len(r.failures) != 0 {
		t.Fatal("a conforming adapter failed:", r.failures)
	}
}

// The declaration rules are not here: the registry enforces them (see the
// registry tests in package agent) and Run reports its refusal, every problem
// of the adapter at once.
func TestRunReportsTheRegistryRefusal(t *testing.T) {
	adapter := good()
	adapter.descriptor.HookEventKey = ""
	adapter.descriptor.Hooks = map[string]string{"stop": "Stop"}
	r := &recorder{}
	checkDeclaration(r, adapter)
	if len(r.failures) != 1 {
		t.Fatalf("want one failure that lists the problems, got %q", r.failures)
	}
	for _, want := range []string{"the registry refuses this adapter's declaration", "fixture maps no native event to SessionStart", "fixture has hooks but declares no HookEventKey"} {
		if !strings.Contains(r.failures[0], want) {
			t.Fatalf("failure lacks %q:\n%s", want, r.failures[0])
		}
	}
}

func TestStartupCheckCatchesBrokenAdapters(t *testing.T) {
	calls := 0
	for _, test := range []struct {
		name    string
		startup func(agent.StartupContext) (any, error)
		want    string
	}{
		{"an error", func(agent.StartupContext) (any, error) { return nil, io.ErrUnexpectedEOF }, "StartupOutput failed for the fixture"},
		{"a panic", func(agent.StartupContext) (any, error) { panic("boom") }, "StartupOutput panicked"},
		{"a value that is not an object", func(v agent.StartupContext) (any, error) { return v.Instructions, nil }, "one non-empty JSON object"},
		{"an empty object", func(agent.StartupContext) (any, error) { return map[string]string{}, nil }, "one non-empty JSON object"},
		{"no instructions", func(agent.StartupContext) (any, error) { return map[string]string{"x": "y"}, nil }, "exactly once"},
		{"instructions twice", func(v agent.StartupContext) (any, error) {
			return map[string]any{"a": v.Instructions, "b": []string{v.Instructions}}, nil
		}, "exactly once"},
		{"a different output each time", func(v agent.StartupContext) (any, error) {
			calls++
			return map[string]any{"context": v.Instructions, "call": calls}, nil
		}, "same bytes for the same input"},
	} {
		adapter := good()
		adapter.startup = test.startup
		r := &recorder{}
		checkStartup(r, adapter, os.DirFS(t.TempDir()), "no-golden")
		if !strings.Contains(strings.Join(r.failures, "\n"), test.want) {
			t.Fatal(test.name, "was not caught:", r.failures)
		}
	}
}

func TestStartupCheckKeepsNestedInstructionsOnce(t *testing.T) {
	adapter := good()
	adapter.startup = func(v agent.StartupContext) (any, error) {
		return map[string]any{"outer": map[string]any{"list": []any{"before " + v.Instructions + " after"}}}, nil
	}
	r := &recorder{}
	checkStartup(r, adapter, os.DirFS(t.TempDir()), "no-golden")
	if len(r.failures) != 0 {
		t.Fatal("instructions inside nested values were refused:", r.failures)
	}
}

func TestStartupCheckComparesBytesWithTheGoldenFile(t *testing.T) {
	adapter := good()
	dir := t.TempDir()
	golden := filepath.Join(dir, "startup-output.jsonl")
	want, err := renderStartup(adapter, startupFixture())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(golden, want, 0o600); err != nil {
		t.Fatal(err)
	}
	r := &recorder{}
	checkStartup(r, adapter, os.DirFS(dir), "startup-output.jsonl")
	if len(r.failures) != 0 {
		t.Fatal("matching golden file was refused:", r.failures)
	}
	if err := os.WriteFile(golden, append([]byte{' '}, want...), 0o600); err != nil {
		t.Fatal(err)
	}
	checkStartup(r, adapter, os.DirFS(dir), "startup-output.jsonl")
	if !strings.Contains(strings.Join(r.failures, "\n"), "differs from") {
		t.Fatal("a different golden file was not caught:", r.failures)
	}
}

func TestStartupCheckSkipsAdaptersWithoutHooks(t *testing.T) {
	r := &recorder{}
	checkStartup(r, plain{}, os.DirFS(t.TempDir()), "no-golden")
	if len(r.failures) != 0 {
		t.Fatal("an adapter without hooks was checked:", r.failures)
	}
}

// plain implements Adapter only, with no hook interface.
type plain struct{}

func (plain) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "plain", Label: "Plain"}
}
func (plain) ReceiveCapabilities(model.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "fixture", Inbox: model.InboxJournal, BusyDelivery: model.DeliveryNextHook}
}
func (plain) Deliver(context.Context, model.Message, model.Session) model.Receipt {
	return model.NewReceipt(model.StatusSubmitted)
}

// Run reads testdata/startup-output.jsonl from the directory of the test. The
// child test below runs Run in a temporary directory with a golden file, so a
// test binary of its own sees the real subtests, the real file name and the
// real current directory.
func TestRunChecksTheGoldenFileOfTheCurrentDirectory(t *testing.T) {
	for _, test := range []struct {
		mode      string
		wantFail  bool
		wantText  string
		wantNoise string
	}{
		{"same", false, "--- PASS: TestRunInATemporaryDirectory/startup", "differs from"},
		{"different", true, "differs from testdata/startup-output.jsonl", ""},
	} {
		cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRunInATemporaryDirectory$", "-test.v")
		cmd.Env = append(os.Environ(), "AGENTTEST_RUN_CHILD="+test.mode)
		out, err := cmd.CombinedOutput()
		if (err != nil) != test.wantFail {
			t.Fatalf("%s golden file: child error = %v, want failure %v\n%s", test.mode, err, test.wantFail, out)
		}
		if !strings.Contains(string(out), test.wantText) || (test.wantNoise != "" && strings.Contains(string(out), test.wantNoise)) {
			t.Fatalf("%s golden file: child output lacks %q or has %q:\n%s", test.mode, test.wantText, test.wantNoise, out)
		}
	}
}

// TestRunInATemporaryDirectory is the child of the test above. It does nothing on its own.
func TestRunInATemporaryDirectory(t *testing.T) {
	mode := os.Getenv("AGENTTEST_RUN_CHILD")
	if mode == "" {
		t.Skip("runs only as the child of TestRunChecksTheGoldenFileOfTheCurrentDirectory")
	}
	golden, err := renderStartup(good(), startupFixture())
	if err != nil {
		t.Fatal(err)
	}
	if mode == "different" {
		golden = append([]byte{' '}, golden...)
	}
	t.Chdir(t.TempDir())
	if err := os.Mkdir("testdata", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(startupGolden, golden, 0o600); err != nil {
		t.Fatal(err)
	}
	Run(t, good())
}
