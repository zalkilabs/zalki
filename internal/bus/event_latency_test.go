package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// This opt-in harness also compiles on the PR1 baseline. Its disk fixture is
// deliberately encoded without new private types so both builds run the same
// source, paths, sample counts and ordinary public Update calls.
func TestEventUpdateLatency(t *testing.T) {
	if os.Getenv("ZALKI_EVENT_LATENCY") != "1" {
		t.Skip("opt-in quiet-window latency harness")
	}
	count, err := strconv.Atoi(os.Getenv("ZALKI_EVENT_LATENCY_SAMPLES"))
	if err != nil || count < 500 || count > 5000 {
		t.Fatal("latency sample count must be 500..5000")
	}
	mode, scenario := os.Getenv("ZALKI_EVENT_LATENCY_MODE"), os.Getenv("ZALKI_EVENT_LATENCY_CASE")
	if mode == "" {
		mode = "covered"
	}
	store, session, key := latencyFixture(t, mode, scenario)
	if os.Getenv("ZALKI_EVENT_LATENCY_PREPARE") == "1" {
		return
	}
	start := time.Now()
	if err := latencyOperation(store, session.ID, key, mode); err != nil {
		t.Fatal(err)
	}
	cold := time.Since(start).Nanoseconds()
	_ = measureUpdateLatency(t, store, session.ID, key, mode, 8)
	samples := measureUpdateLatency(t, store, session.ID, key, mode, count)
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	report := map[string]any{"mode": mode, "scenario": scenario, "samples": count, "cold_ns": cold,
		"median_ns": float64(sorted[len(sorted)/2-1]+sorted[len(sorted)/2]) / 2, "p95_ns": sorted[(95*len(sorted)+99)/100-1], "raw_ns": samples}
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("LATENCY_JSON %s", raw)
}

func latencyFixture(t *testing.T, mode, scenario string) (Store, model.Session, string) {
	t.Helper()
	store := fixtureStore(t)
	if root := os.Getenv("ZALKI_EVENT_LATENCY_SHARED"); root != "" {
		store.Dir = root
		if os.Getenv("ZALKI_EVENT_LATENCY_PREPARE") != "1" {
			sessions, err := store.List()
			if err != nil || len(sessions) != 1 {
				t.Fatal("bad shared latency fixture", err)
			}
			return store, sessions[0], ""
		}
	}
	session, key := register(t, store, "codex")
	switch {
	case scenario == "full":
		seedLatencyRecords(t, store, session, 4096)
	case scenario != "nonfull":
		t.Fatal("latency case must be nonfull or full")
	case mode != "covered":
		seedLatencyRecords(t, store, session, 1)
	}
	if mode != "covered" {
		if err := store.Update(session.ID, model.StateBusy, false); err != nil {
			t.Fatal(err)
		}
	}
	return store, session, key
}

func latencyOperation(store Store, id, key, mode string) error {
	switch mode {
	case "covered", "busy":
		return store.Update(id, model.StateBusy, false)
	case "read":
		_, err := store.List()
		return err
	case "write":
		_, err := (&Service{Store: store}).SetTask(id, key, "latency")
		return err
	default:
		return fmt.Errorf("unknown latency mode %q", mode)
	}
}

func measureUpdateLatency(t *testing.T, store Store, id, key, mode string, count int) []int64 {
	t.Helper()
	samples := make([]int64, 0, count)
	for range count {
		if mode == "covered" {
			if err := store.Update(id, model.StateReady, false); err != nil {
				t.Fatal(err)
			}
		}
		start := time.Now()
		if err := latencyOperation(store, id, key, mode); err != nil {
			t.Fatal(err)
		}
		samples = append(samples, time.Since(start).Nanoseconds())
	}
	return samples
}

func seedLatencyRecords(t *testing.T, store Store, session model.Session, count uint64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(store.Dir, "commits"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(store.Dir, "events", "records"), 0700); err != nil {
		t.Fatal(err)
	}
	epoch := "00000000-0000-4000-8000-000000000099"
	var total uint64
	for sequence := uint64(1); sequence <= count; sequence++ {
		event := model.Event{ID: fmt.Sprintf("10000000-0000-4000-8000-%012x", sequence), Type: "session.state_changed",
			OccurredAt: "2026-11-07T11:00:00Z", TransactionID: fmt.Sprintf("20000000-0000-4000-8000-%012x", sequence),
			Session: model.EventSession{ID: session.ID, Generation: session.Generation, Previous: model.StateReady, State: model.StateBusy}}
		record := map[string]any{"epoch": epoch, "sequence": strconv.FormatUint(sequence, 10), "event": map[string]any{"id": event.ID, "type": event.Type, "occurred_at": event.OccurredAt, "transaction_id": event.TransactionID, "session": event.Session}}
		raw, err := journal.EncodeJSON(record, 4096)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(store.Dir, "events", "records", fmt.Sprintf("%020d.json", sequence))
		if err := journal.WriteAtomic(path, raw); err != nil {
			t.Fatal(err)
		}
		total += uint64(len(raw))
	}
	metadata := map[string]any{"epoch": epoch, "first_sequence": "1", "last_sequence": strconv.FormatUint(count, 10), "count": count, "bytes": total}
	raw, err := journal.EncodeJSON(metadata, 128<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(filepath.Join(store.Dir, "events", "meta.json"), raw); err != nil {
		t.Fatal(err)
	}
}
