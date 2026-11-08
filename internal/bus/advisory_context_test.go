package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
	"golang.org/x/sys/unix"
)

type advisoryFixture struct {
	store                Store
	path, root, captures string
}

func newAdvisoryFixture(t *testing.T) advisoryFixture {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "codex")
	// Built-ins keep the blocked client in one process without starting the
	// race-instrumented test binary inside the readiness deadline.
	script := `#!/bin/sh
printf yes > "$ZALKI_TEST_VERSION_CHILD/launched" || exit 2
if [ -n "$ZALKI_TEST_VERSION_GATE" ]; then
    printf 1 > "$ZALKI_TEST_VERSION_GATE-ready" || exit 3
    IFS= read -r signal < "$ZALKI_TEST_VERSION_GATE-release" || exit 4
fi
printf 'client 0.163.0\n'
`
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZALKI_TEST_VERSION_CHILD", root)
	t.Setenv("ZALKI_TEST_VERSION_GATE", "")
	t.Setenv("PATH", root)
	captures := filepath.Join(root, "captures")
	t.Setenv("ZALKI_CAPTURE_CONTRACTS", captures)
	registry, err := agent.NewRegistry(stubCodexAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	store := fixtureStore(t)
	store.Registry = registry
	return advisoryFixture{store: store, path: path, root: root, captures: captures}
}

type advisoryResult struct {
	version string
	err     error
}

func (f advisoryFixture) run(ctx context.Context, chain string) advisoryResult {
	switch chain {
	case "probe":
		return advisoryResult{version: probeClientVersion(ctx, f.path)}
	case "diagnostics":
		return f.diagnostics(ctx)
	case "hook":
		if err := f.runHook(ctx); err != nil {
			return advisoryResult{err: err}
		}
	default:
		return advisoryResult{err: fmt.Errorf("unknown chain %s", chain)}
	}
	paths, err := filepath.Glob(filepath.Join(f.captures, "*.json"))
	if err != nil || len(paths) != 1 {
		return advisoryResult{err: fmt.Errorf("captures=%d: %w", len(paths), err)}
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		return advisoryResult{err: err}
	}
	var fixture struct{ Version string }
	if err := json.Unmarshal(raw, &fixture); err != nil {
		return advisoryResult{err: err}
	}
	if bytes.Contains(raw, []byte("private prompt")) || bytes.Contains(raw, []byte("/private/workspace")) {
		return advisoryResult{err: errors.New("capture leaked private fields")}
	}
	return advisoryResult{version: fixture.Version}
}

func (f advisoryFixture) diagnostics(ctx context.Context) advisoryResult {
	report, err := f.store.Diagnostics(ctx)
	if err != nil {
		return advisoryResult{err: err}
	}
	if len(report.Agents) != 1 {
		return advisoryResult{err: fmt.Errorf("agents=%d", len(report.Agents))}
	}
	diagnostic := report.Agents[0]
	want := "fixture_covered"
	if diagnostic.Version == "" {
		want = "version_unavailable"
	}
	if !diagnostic.Found || diagnostic.Compatibility != want || len(diagnostic.FixtureVersions) == 0 || diagnostic.RecoveryHint == "" {
		return advisoryResult{err: errors.New("advisory diagnosis lost installed-client status or fixture evidence")}
	}
	return advisoryResult{version: diagnostic.Version}
}

func (f advisoryFixture) runHook(ctx context.Context) error {
	service := Service{Store: f.store}
	native := newID()
	input := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"cwd":"/private/workspace","prompt":"private prompt"}`, native)
	var output bytes.Buffer
	if err := service.Hook(ctx, "codex", strings.NewReader(input), &output); err != nil {
		return err
	}
	id, err := f.store.sessionID("codex", native)
	if err != nil {
		return err
	}
	session, err := f.store.session(id)
	if err != nil {
		return err
	}
	if session.State != model.StateReady || session.KeyHash == "" || session.Generation == "" || !bytes.Contains(output.Bytes(), []byte(id)) {
		return errors.New("advisory probe interrupted startup state, credential or output")
	}
	return nil
}

type advisoryGate struct{ ready, release *os.File }

func (f advisoryFixture) gate(t *testing.T) advisoryGate {
	t.Helper()
	gate := filepath.Join(f.root, "gate")
	pipes := make([]*os.File, 0, 2)
	for _, suffix := range []string{"-ready", "-release"} {
		path := gate + suffix
		if err := unix.Mkfifo(path, 0600); err != nil {
			t.Fatal(err)
		}
		pipe, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pipe.Close() })
		pipes = append(pipes, pipe)
	}
	t.Setenv("ZALKI_TEST_VERSION_GATE", gate)
	return advisoryGate{ready: pipes[0], release: pipes[1]}
}

func (g advisoryGate) awaitReady(t *testing.T) {
	t.Helper()
	done := make(chan error, 1)
	go func() { var signal [1]byte; _, err := io.ReadFull(g.ready, signal[:]); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		_ = g.ready.Close()
		<-done
		t.Fatal("version child never reached gate")
	}
}

func awaitAdvisoryResult(t *testing.T, done <-chan advisoryResult) advisoryResult {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(3 * time.Second):
		t.Fatal("advisory operation did not finish")
		return advisoryResult{}
	}
}

func TestAdvisoryProbeInheritsCanceledParentBeforeLaunch(t *testing.T) {
	for _, chain := range []string{"probe", "diagnostics", "hook"} {
		t.Run(chain, func(t *testing.T) {
			f := newAdvisoryFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			result := f.run(ctx, chain)
			if result.err != nil || result.version != advisoryCanceledVersion(chain) {
				t.Fatalf("result=%+v", result)
			}
			if _, err := os.Stat(filepath.Join(f.root, "launched")); !os.IsNotExist(err) {
				t.Fatal("canceled parent launched a child", err)
			}
		})
	}
}

func TestAdvisoryProbeInheritsParentCancellationDuringExecution(t *testing.T) {
	for _, chain := range []string{"probe", "diagnostics", "hook"} {
		t.Run(chain, func(t *testing.T) {
			f := newAdvisoryFixture(t)
			gate := f.gate(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan advisoryResult, 1)
			finished := make(chan struct{})
			go func() { defer close(finished); done <- f.run(ctx, chain) }()
			t.Cleanup(func() {
				_ = gate.release.Close()
				select {
				case <-finished:
				case <-time.After(7 * time.Second):
					t.Error("advisory child cleanup did not finish")
				}
			})
			gate.awaitReady(t)
			assertAdvisoryJournalUnlocked(t, f.store)
			cancel()
			result := awaitAdvisoryResult(t, done)
			if result.err != nil || result.version != advisoryCanceledVersion(chain) {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func assertAdvisoryJournalUnlocked(t *testing.T, store Store) {
	t.Helper()
	done := make(chan advisoryResult, 1)
	go func() { _, err := store.List(); done <- advisoryResult{err: err} }()
	if result := awaitAdvisoryResult(t, done); result.err != nil {
		t.Fatal(result.err)
	}
}

func TestAdvisoryProbeBackgroundControl(t *testing.T) {
	for _, chain := range []string{"probe", "diagnostics", "hook"} {
		t.Run(chain, func(t *testing.T) {
			f := newAdvisoryFixture(t)
			result := f.run(context.Background(), chain)
			if result.err != nil || result.version != "0.163.0" {
				t.Fatalf("result=%+v", result)
			}
		})
	}
}

func advisoryCanceledVersion(chain string) string {
	if chain == "hook" {
		return "unknown"
	}
	return ""
}

func TestAdvisoryProbeParentDeadlineAndLocalCeiling(t *testing.T) {
	for _, test := range []struct {
		name                 string
		parentLimit, maximum time.Duration
	}{
		{"shorter_parent", 1500 * time.Millisecond, 3 * time.Second},
		{"local_ceiling", 20 * time.Second, 7 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newAdvisoryFixture(t)
			gate := f.gate(t)
			ctx, cancel := context.WithTimeout(t.Context(), test.parentLimit)
			defer cancel()
			started := time.Now()
			if version := probeClientVersion(ctx, f.path); version != "" {
				t.Fatalf("blocked probe version=%q", version)
			}
			gate.awaitReady(t)
			if elapsed := time.Since(started); elapsed > test.maximum {
				t.Fatalf("probe took %s, limit %s", elapsed, test.maximum)
			}
			if test.name == "shorter_parent" && ctx.Err() != context.DeadlineExceeded {
				t.Fatal("shorter parent deadline was not the stopping condition")
			}
			if test.name == "local_ceiling" && ctx.Err() != nil {
				t.Fatal("probe exhausted the longer parent")
			}
		})
	}
}

func TestAdvisoryProbeCancellationKeepsDiagnosticsReadOnly(t *testing.T) {
	f := newAdvisoryFixture(t)
	session, key := register(t, f.store, "codex")
	before := journalSnapshot(t, f.store)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	report, err := f.store.Diagnostics(ctx)
	if err != nil || len(report.Sessions) != 1 || report.Sessions[0].ID != session.ID || report.Agents[0].Compatibility != "version_unavailable" {
		t.Fatalf("advisory cancellation interrupted report: err=%v sessions=%d", err, len(report.Sessions))
	}
	if after := journalSnapshot(t, f.store); !reflect.DeepEqual(after, before) {
		t.Fatal("advisory cancellation changed journal or credentials")
	}
	if _, err := f.store.authenticate(session.ID, key); err != nil {
		t.Fatal("advisory cancellation invalidated credential", err)
	}
}

func TestAdvisoryProbeCancellationKeepsCaptureValidationOrder(t *testing.T) {
	f := newAdvisoryFixture(t)
	t.Setenv("ZALKI_CAPTURE_CONTRACTS", "relative")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	service := Service{Store: f.store}
	input := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q}`, newID())
	err := service.Hook(ctx, "codex", strings.NewReader(input), io.Discard)
	if err == nil || err.Error() != "ZALKI_CAPTURE_CONTRACTS must be an absolute private directory" {
		t.Fatalf("cancellation bypassed capture validation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.root, "launched")); !os.IsNotExist(err) {
		t.Fatal("validation failure launched the advisory child", err)
	}
	sessions, err := f.store.List()
	if err != nil || len(sessions) != 0 {
		t.Fatal("failed capture created a session", err)
	}
}
