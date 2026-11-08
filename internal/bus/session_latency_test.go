package bus

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

type sessionLatencySubject struct {
	store      Store
	session    Session
	key        string
	lease      *Lease
	activation int64
}

// This harness is copied unchanged to the accepted PR2 baseline. Fixtures and
// reset work are outside each timed call; raw values are never filtered.
func TestSessionPathLatency(t *testing.T) {
	if os.Getenv("ZALKI_SESSION_LATENCY") != "1" {
		t.Skip("opt-in quiet-window public session-path benchmark")
	}
	scenario := os.Getenv("ZALKI_SESSION_LATENCY_CASE")
	if os.Getenv("ZALKI_SESSION_LATENCY_PREPARE") == "1" {
		prepareSessionLatencySeed(t, scenario)
		return
	}
	count, err := strconv.Atoi(os.Getenv("ZALKI_SESSION_LATENCY_SAMPLES"))
	if err != nil || (count < 500 && os.Getenv("ZALKI_SESSION_LATENCY_SMOKE") != "1") || count < 1 || count > 5000 {
		t.Fatal("sample count must be500..5000 (smoke permits1)")
	}
	mode := os.Getenv("ZALKI_SESSION_LATENCY_MODE")
	subject := newSessionLatencySubject(t, mode, scenario)
	cold := sampleSessionPath(t, &subject, mode)
	if os.Getenv("ZALKI_SESSION_LATENCY_SMOKE") != "1" {
		for range 8 {
			_ = sampleSessionPath(t, &subject, mode)
		}
	}
	samples := make([]int64, 0, count)
	for range count {
		samples = append(samples, sampleSessionPath(t, &subject, mode))
	}
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	median := float64(sorted[len(sorted)/2])
	if len(sorted)%2 == 0 {
		median = float64(sorted[len(sorted)/2-1]+sorted[len(sorted)/2]) / 2
	}
	raw, err := json.Marshal(map[string]any{"mode": mode, "scenario": scenario, "samples": count, "cold_ns": cold, "activation_ns": subject.activation, "median_ns": median, "p95_ns": sorted[(95*len(sorted)+99)/100-1], "raw_ns": samples})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("SESSION_LATENCY_JSON %s", raw)
}

func newSessionLatencySubject(t *testing.T, mode, scenario string) sessionLatencySubject {
	t.Helper()
	store := fixtureStore(t)
	kind := "codex"
	if strings.HasPrefix(mode, "adapter-") || mode == "listener" || mode == "recovery" {
		kind = "cursor"
	}
	start := time.Now()
	session, key := register(t, store, kind)
	activation := time.Since(start).Nanoseconds()
	if seed := os.Getenv("ZALKI_SESSION_LATENCY_SEED"); seed != "" {
		cloneSessionLatencyEvents(t, store.Dir, filepath.Join(seed, scenario))
	}
	subject := sessionLatencySubject{store: store, session: session, key: key, activation: activation}
	if mode == "listener" {
		subject.lease = armedLease(t, store, session.ID, key)
	}
	return subject
}

func sampleSessionPath(t *testing.T, s *sessionLatencySubject, mode string) int64 {
	t.Helper()
	if err := s.reset(mode); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := s.run(t, mode)
	elapsed := time.Since(start).Nanoseconds()
	if err != nil {
		t.Fatal(err)
	}
	return elapsed
}

func (s *sessionLatencySubject) reset(mode string) error {
	switch mode {
	case "busy", "approval", "adapter-busy", "hook-permission":
		return s.store.Update(s.session.ID, model.StateReady, false)
	case "ready", "adapter-ready", "hook-stop":
		return s.store.Update(s.session.ID, model.StateBusy, false)
	case "retire", "hook-end":
		next, key, err := s.store.Register(s.session.Kind, model.NativeID(s.session), s.session.CWD, nil)
		if err == nil {
			s.session, s.key = next, key
		}
		return err
	case "same-state":
		return s.store.Update(s.session.ID, model.StateBusy, false)
	case "count":
		return s.store.locked(func() error { return s.store.clearApprovalHeld(s.session.ID) })
	default:
		return nil
	}
}

func (s *sessionLatencySubject) run(t *testing.T, mode string) error {
	t.Helper()
	switch mode {
	case "register-new", "register-replace":
		native := model.NativeID(s.session)
		if mode == "register-new" {
			native = newID()
		}
		_, _, err := s.store.Register(s.session.Kind, native, s.session.CWD, nil)
		return err
	case "busy", "same-state", "ready", "approval", "retire":
		return s.runState(mode)
	case "adapter-busy", "adapter-ready":
		return s.runAdapter(mode)
	case "hook-start", "hook-permission", "hook-stop", "hook-end":
		return s.runHook(t, mode)
	default:
		return s.runPrivate(mode)
	}
}

func (s *sessionLatencySubject) runState(mode string) error {
	state := map[string]model.SessionState{"busy": model.StateBusy, "same-state": model.StateBusy, "ready": model.StateReady, "approval": model.StateApproval, "retire": model.StateRetired}[mode]
	return s.store.Update(s.session.ID, state, mode == "retire")
}

func (s *sessionLatencySubject) runPrivate(mode string) error {
	switch mode {
	case "list":
		_, err := s.store.List()
		return err
	case "task":
		_, err := (&Service{Store: s.store}).SetTask(s.session.ID, s.key, "latency")
		return err
	case "count":
		return s.store.locked(func() error { return s.store.rememberApprovalHold(s.session.ID, 1) })
	case "listener":
		return s.lease.Confirm(func(model.SessionView) (json.RawMessage, error) { return nil, nil })
	case "recovery":
		_, err := cursorHost(s.store, s.session.ID, "PostToolUse").ClaimRecovery(func(model.SessionView) (agent.RecoveryDecision, error) {
			return agent.RecoveryDecision{Recover: true}, nil
		})
		return err
	default:
		return fmt.Errorf("unknown session latency mode %q", mode)
	}
}

func (s *sessionLatencySubject) runAdapter(mode string) error {
	event, transition := "UserPromptSubmit", agent.TransitionPromptReactivate
	if mode == "adapter-ready" {
		event, transition = "Stop", agent.TransitionReady
	}
	return cursorHost(s.store, s.session.ID, event).ApplyLifecycle(func(model.SessionView) (agent.LifecycleChange, error) {
		return agent.LifecycleChange{Transition: transition}, nil
	})
}

func (s *sessionLatencySubject) runHook(t *testing.T, mode string) error {
	t.Helper()
	event := map[string]string{"hook-start": "SessionStart", "hook-permission": "PermissionRequest", "hook-stop": "Stop", "hook-end": "SessionEnd"}[mode]
	input, err := json.Marshal(map[string]any{"hook_event_name": event, "session_id": model.NativeID(s.session), "cwd": s.session.CWD, "session_title": "latency-title", "status": "completed"})
	if err != nil {
		return err
	}
	return (&Service{Store: s.store}).Hook(t.Context(), s.session.Kind, strings.NewReader(string(input)), io.Discard)
}

// Seed once before the quiet window. Each run hard-links these already durable,
// immutable event bytes into its private namespace. Production replaces via
// rename or unlinks; it never modifies an inode shared with this template.
func prepareSessionLatencySeed(t *testing.T, scenario string) {
	t.Helper()
	root := os.Getenv("ZALKI_SESSION_LATENCY_SEED")
	if root == "" {
		t.Fatal("seed root required")
	}
	count := uint64(1)
	if scenario == "full" {
		count = 4096
	} else if scenario != "nonfull" {
		t.Fatal("invalid seed case")
	}
	store := Store{Dir: filepath.Join(root, scenario)}
	session := Session{ID: "codex:00000000-0000-4000-8000-000000000003", Generation: "00000000-0000-4000-8000-000000000004"}
	seedLatencyRecords(t, store, session, count)
	for _, relative := range []string{"events/records", "events", "commits", "."} {
		if err := journal.SyncDir(filepath.Join(store.Dir, relative)); err != nil {
			t.Fatal(err)
		}
	}
}

func cloneSessionLatencyEvents(t *testing.T, target, source string) {
	t.Helper()
	for _, relative := range []string{"commits", "events", "events/records"} {
		if err := privateDir(filepath.Join(target, relative)); err != nil {
			t.Fatal(err)
		}
	}
	records := filepath.Join(target, "events", "records")
	existing, err := os.ReadDir(records)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range existing {
		if err := os.Remove(filepath.Join(records, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(source, "events", "records"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if err := os.Link(filepath.Join(source, "events", "records", entry.Name()), filepath.Join(records, entry.Name())); err != nil {
			t.Fatal(err)
		}
	}
	metadata := filepath.Join(target, eventMetaPath)
	if err := os.Remove(metadata); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(source, eventMetaPath), metadata); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []string{"events/records", "events", "commits", "."} {
		if err := journal.SyncDir(filepath.Join(target, relative)); err != nil {
			t.Fatal(err)
		}
	}
}
