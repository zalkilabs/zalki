package bus

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type smokeSignals struct {
	mu       sync.Mutex
	buffer   string
	ready    chan struct{}
	messages chan string
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
			if _, err := UUID(id); err != nil {
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

type smokeObservation struct {
	Client     string         `json:"client"`
	Version    string         `json:"installed_version,omitempty"`
	Scenario   string         `json:"scenario"`
	Evidence   string         `json:"evidence"`
	Status     string         `json:"status"`
	Receipt    DeliveryStatus `json:"receipt,omitempty"`
	ReplyBytes int            `json:"reply_bytes,omitempty"`
	LatencyMS  int64          `json:"latency_ms,omitempty"`
}

type liveSmokeReport struct {
	StartedAt    string             `json:"started_at"`
	Build        string             `json:"build"`
	Passed       bool               `json:"passed"`
	Observations []smokeObservation `json:"observations"`
	ModelTokens  *int               `json:"model_tokens"`
	ModelTurns   *int               `json:"model_turns"`
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
	store := Store{Dir: state}
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
	selected := map[string]Session{}
	for _, id := range targets {
		var session Session
		if err := store.locked(func() error { var err error; session, err = store.session(id); return err }); err != nil {
			t.Fatal(err)
		}
		cwd, err := filepath.EvalSymlinks(session.CWD)
		if err != nil || cwd != workspace || session.Retired || processLiveness(session.Owner) == "exited" {
			t.Fatalf("%s must be a live disposable session in the smoke workspace", id)
		}
		selected[id] = session
	}
	report := liveSmokeReport{StartedAt: timestamp(), Build: BuildVersion(), Observations: []smokeObservation{}}
	reportDir := filepath.Join(state, "smoke-reports")
	if err := privateDir(reportDir); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(reportDir, newID()+".json")
	t.Cleanup(func() {
		report.Passed = !t.Failed()
		if err := writeJSON(reportPath, report); err != nil {
			t.Error(err)
		}
		t.Log("private smoke report:", reportPath)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	source, key, err := store.Register("cursor", newID(), workspace, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Update(source.ID, StateRetired, true) })
	signals := &smokeSignals{ready: make(chan struct{}, 1), messages: make(chan string, 16)}
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
	service := Service{Store: store}
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
				if err := store.locked(func() error { var err error; current, err = store.session(id); return err }); err != nil {
					t.Fatal(err)
				}
				ownerChanged := current.Owner != nil && previous.Owner != nil && (current.Owner.PID != previous.Owner.PID || current.Owner.Start != previous.Owner.Start)
				if current.Generation == previous.Generation && !ownerChanged {
					t.Fatal("restart could not be verified: neither registration nor owner fingerprint changed")
				}
				evidence = "registration_or_owner_change_and_model_reply"
			case "draft-preservation":
				confirm("Type ZALKI_SMOKE_DRAFT_SENTINEL in " + id + " and leave it unsubmitted")
				evidence = "model_reply_and_operator_draft_check"
			}
			if err := store.locked(func() error {
				var err error
				current, err = store.session(id)
				return err
			}); err != nil {
				t.Fatal(err)
			}
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
			adapter, _ := store.registry().Lookup(current.Kind)
			if path, err := execClientPath(adapter.Descriptor().Executable); err == nil {
				observation.Version = probeClientVersion(path)
			}
			challenge := "ZALKI_SMOKE_" + newID()
			body := challenge + "\nOpt-in disposable-session review: a local message bus records transport acceptance separately from model acknowledgement. State one failure mode in one sentence. Use reply_message once, with a body beginning " + challenge + ": followed by your review. Use normal permissions; make no file/config changes and send no extra acknowledgement-only messages."
			started := time.Now()
			request, err := service.Send(ctx, SendRequest{Actor: source.ID, Capability: key, To: id, MessageID: newID(), Body: body})
			observation.Receipt = request.Status
			if err != nil || !request.Status.canAcknowledge() {
				report.Observations[observationIndex] = observation
				t.Fatal("smoke send was not accepted:", request.Status, err)
			}
			timer := time.NewTimer(2 * time.Minute)
			var reply Message
			select {
			case replyID := <-signals.messages:
				batch, err := store.ReadMessages(MessageQuery{Actor: source.ID, Capability: key, MessageID: replyID})
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
			original, err := store.ReadMessages(MessageQuery{Actor: source.ID, Capability: key, MessageID: request.ID})
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
