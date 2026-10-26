package bus_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/fsutil"
)

// Live smoke composes the real adapter registry from outside package bus.
// Core tests keep a Codex stub, which cannot execute the native queue.

type smokeObservation struct {
	Client     string             `json:"client"`
	Version    string             `json:"installed_version,omitempty"`
	Scenario   string             `json:"scenario"`
	Evidence   string             `json:"evidence"`
	Status     string             `json:"status"`
	Receipt    bus.DeliveryStatus `json:"receipt,omitempty"`
	ReplyBytes int                `json:"reply_bytes,omitempty"`
	LatencyMS  int64              `json:"latency_ms,omitempty"`
}

type liveSmokeReport struct {
	StartedAt    string             `json:"started_at"`
	Build        string             `json:"build"`
	Passed       bool               `json:"passed"`
	Observations []smokeObservation `json:"observations"`
	ModelTokens  *int               `json:"model_tokens"`
	ModelTurns   *int               `json:"model_turns"`
}

type smokeSignals struct {
	mu       sync.Mutex
	buffer   string
	ready    chan struct{}
	messages chan string
}

func newSmokeSignals() *smokeSignals {
	return &smokeSignals{ready: make(chan struct{}, 1), messages: make(chan string, 16)}
}

func (s *smokeSignals) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buffer += string(p)
	for {
		line, rest, ok := strings.Cut(s.buffer, "\n")
		if !ok {
			break
		}
		s.buffer = rest
		if strings.HasPrefix(line, "ZALKI_LISTENER_READY ") {
			select {
			case s.ready <- struct{}{}:
			default:
			}
		}
		if id, ok := strings.CutPrefix(line, "ZALKI_WAKE "); ok {
			if _, err := bus.UUID(id); err != nil {
				return 0, err
			}
			select {
			case s.messages <- id:
			default:
				return 0, fmt.Errorf("smoke notification queue is full")
			}
		}
	}
	return len(p), nil
}

func TestLiveClients(t *testing.T) {
	if os.Getenv("ZALKI_LIVE_SMOKE") != "1" {
		t.Skip("opt in with make smoke-live; this sends real model requests")
	}
	state, workspace := os.Getenv("ZALKI_SMOKE_STATE"), os.Getenv("ZALKI_SMOKE_WORKSPACE")
	if !filepath.IsAbs(state) || !filepath.IsAbs(workspace) {
		t.Fatal("set absolute ZALKI_SMOKE_STATE and ZALKI_SMOKE_WORKSPACE")
	}
	workspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	targets := strings.Fields(strings.ReplaceAll(os.Getenv("ZALKI_SMOKE_TARGETS"), ",", " "))
	if len(targets) == 0 {
		t.Fatal("set ZALKI_SMOKE_TARGETS to exact disposable-session IDs")
	}
	store := bus.Store{Dir: state, Registry: adapters.Builtin()}
	scenarios := strings.Split(os.Getenv("ZALKI_SMOKE_CASES"), ",")
	if len(scenarios) == 1 && scenarios[0] == "" {
		scenarios = []string{"fresh-registration", "idle-wake", "restart-recovery", "draft-preservation"}
	}
	for _, scenario := range scenarios {
		switch scenario {
		case "fresh-registration", "idle-wake", "restart-recovery", "draft-preservation", "roundtrip":
		default:
			t.Fatalf("unknown smoke case %q", scenario)
		}
	}
	selected := map[string]bus.Session{}
	for _, id := range targets {
		session := smokeSession(t, store, id)
		cwd, err := filepath.EvalSymlinks(session.CWD)
		if err != nil || cwd != workspace || session.Retired || session.Liveness == "exited" {
			t.Fatalf("%s must be a live disposable session in the smoke workspace", id)
		}
		selected[id] = session
	}
	report := liveSmokeReport{StartedAt: time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z"), Build: bus.BuildVersion(), Observations: []smokeObservation{}}
	reportDir := filepath.Join(state, "smoke-reports")
	if err := os.MkdirAll(reportDir, 0700); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(reportDir, smokeID()+".json")
	t.Cleanup(func() {
		report.Passed = !t.Failed()
		if err := writeSmokeReport(reportPath, report); err != nil {
			t.Error(err)
		}
		t.Log("private smoke report:", reportPath)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source, key, err := store.Register("cursor", smokeID(), workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Update(source.ID, bus.StateRetired, true) })
	signals := newSmokeSignals()
	listenerDone := make(chan struct{})
	var listenerErr error
	go func() { listenerErr = store.ListenCursor(ctx, source.ID, key, signals); close(listenerDone) }()
	defer func() {
		cancel()
		select {
		case <-listenerDone:
		case <-time.After(5 * time.Second):
			t.Error("smoke receiver did not stop")
		}
	}()
	select {
	case <-signals.ready:
	case <-listenerDone:
		t.Fatal(listenerErr)
	case <-time.After(5 * time.Second):
		t.Fatal("smoke receiver did not arm")
	}
	service := bus.Service{Store: store}
	input := bufio.NewReader(os.Stdin)
	confirm := func(prompt string) {
		t.Helper()
		fmt.Fprint(os.Stderr, prompt+" [yes/no]: ")
		line, err := input.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "yes" {
			t.Fatal("operator observation not confirmed; no pass recorded")
		}
	}
	for _, id := range targets {
		previous := selected[id]
		for _, scenario := range scenarios {
			current := previous
			evidence := "transport_and_model_reply"
			switch scenario {
			case "fresh-registration":
				confirm("Confirm " + id + " was freshly started in the disposable workspace")
				evidence = "operator_fresh_start_and_model_reply"
			case "idle-wake":
				confirm("Leave " + id + " idle, with any required listener armed, then confirm")
				evidence = "operator_idle_and_model_reply"
			case "restart-recovery":
				confirm("Restart/resume " + id + ", let startup finish, then confirm")
				current = smokeSession(t, store, id)
				ownerChanged := current.Owner != nil && previous.Owner != nil && (current.Owner.PID != previous.Owner.PID || current.Owner.Start != previous.Owner.Start)
				if current.Generation == previous.Generation && !ownerChanged {
					t.Fatal("restart could not be verified: neither registration nor owner fingerprint changed")
				}
				evidence = "registration_or_owner_change_and_model_reply"
			case "draft-preservation":
				confirm("Type ZALKI_SMOKE_DRAFT_SENTINEL in " + id + " and leave it unsubmitted")
				evidence = "model_reply_and_operator_draft_check"
			}
			current = smokeSession(t, store, id)
			cwd, err := filepath.EvalSymlinks(current.CWD)
			if err != nil || cwd != workspace || current.Retired {
				t.Fatal("target left the disposable smoke workspace")
			}
			if scenario == "idle-wake" && !current.Receive.IdleWake {
				t.Fatal("target does not advertise idle wake; arm its native listener first")
			}
			observation := smokeObservation{Client: current.Kind, Scenario: scenario, Evidence: evidence, Status: "failed"}
			report.Observations = append(report.Observations, observation)
			observationIndex := len(report.Observations) - 1
			if adapter, err := store.Registry.Lookup(current.Kind); err == nil {
				observation.Version = smokeClientVersion(adapter.Descriptor().Executable)
			}
			challenge := "ZALKI_SMOKE_" + smokeID()
			body := challenge + "\nOpt-in disposable-session review: a local message bus records transport acceptance separately from model acknowledgement. State one failure mode in one sentence. Use reply_message once, with a body beginning " + challenge + ": followed by your review. Use normal permissions; make no file/config changes and send no extra acknowledgement-only messages."
			started := time.Now()
			request, err := service.Send(ctx, bus.SendRequest{Actor: source.ID, Capability: key, To: id, MessageID: smokeID(), Body: body})
			observation.Receipt = request.Status
			if err != nil || !smokeAccepted(request.Status) {
				report.Observations[observationIndex] = observation
				t.Fatal("smoke send was not accepted:", request.Status, err)
			}
			timer := time.NewTimer(2 * time.Minute)
			var reply bus.Message
			select {
			case replyID := <-signals.messages:
				batch, err := store.ReadMessages(bus.MessageQuery{Actor: source.ID, Capability: key, MessageID: replyID})
				if err != nil || len(batch.Messages) != 1 {
					t.Fatal("could not read smoke reply", err)
				}
				reply = batch.Messages[0]
			case <-timer.C:
				report.Observations[observationIndex] = observation
				t.Fatal("no model reply within two minutes; original request was not resent")
			case <-listenerDone:
				t.Fatal("smoke receiver stopped:", listenerErr)
			}
			timer.Stop()
			if reply.From != id || reply.ReplyTo != request.ID || !strings.HasPrefix(reply.Body, challenge+":") || len(strings.TrimSpace(strings.TrimPrefix(reply.Body, challenge+":"))) < 10 {
				t.Fatal("reply failed exact routing, correlation, or substantive-content check")
			}
			original, err := store.ReadMessages(bus.MessageQuery{Actor: source.ID, Capability: key, MessageID: request.ID})
			if err != nil || len(original.Messages) != 1 || original.Messages[0].AcknowledgedAt == "" {
				t.Fatal("model reply did not acknowledge the request", err)
			}
			if _, err := store.Ack(source.ID, key, reply.ID); err != nil {
				t.Fatal(err)
			}
			if scenario == "draft-preservation" {
				confirm("Confirm the exact draft sentinel remains unchanged and unsubmitted in " + id)
			}
			observation.Status, observation.ReplyBytes, observation.LatencyMS = "passed", len(reply.Body), time.Since(started).Milliseconds()
			report.Observations[observationIndex] = observation
			previous = current
		}
	}
	encoded, _ := json.Marshal(report.Observations)
	t.Log(string(encoded))
}

func smokeSession(t *testing.T, store bus.Store, id string) bus.Session {
	t.Helper()
	sessions, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, session := range sessions {
		if session.ID == id {
			return session
		}
	}
	t.Fatalf("%s is not a live registered session", id)
	return bus.Session{}
}

func smokeAccepted(status bus.DeliveryStatus) bool {
	switch status {
	case bus.StatusAttempting, bus.StatusSubmitted, bus.StatusTransportWritten, bus.StatusPendingActivity,
		bus.StatusWakeWritten, bus.StatusWakeUncertain, bus.StatusContextEmitted,
		bus.StatusUnknownTimeout, bus.StatusUnknownWrite:
		return true
	default:
		return false
	}
}

func smokeID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	b[6], b[8] = b[6]&15|64, b[8]&63|128
	h := hex.EncodeToString(b)
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func smokeClientVersion(name string) string {
	path, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--version")
	command.WaitDelay = time.Second
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return ""
	}
	text := output.String()
	if len(text) > 1024 {
		text = text[:1024]
	}
	return regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?`).FindString(text)
}

func writeSmokeReport(path string, report liveSmokeReport) error {
	b, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, append(b, '\n'))
}
