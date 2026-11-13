package bus_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// TestLeaseOperationsAndPruneDoNotDeadlock pins both lock orders. Each cycle
// keeps the lifetime lock until prune and list have returned, so a blocking
// journal-to-lifetime probe cannot pass merely by missing the overlap.
func TestLeaseOperationsAndPruneDoNotDeadlock(t *testing.T) {
	if os.Getenv("ZALKI_TEST_LEASE_LOCK_CYCLES") == "1" {
		store := newStore(t, listenerAdapter("cursor"))
		a := register(t, store, "cursor")
		other := restart(t, store)
		endpoint, err := bus.InspectEndpoint(os.Getenv("ZALKI_TEST_LEASE_ENDPOINT"))
		if err != nil {
			t.Fatal(err)
		}
		// Current prune probes lifetime locks only for retired/orphan records;
		// List only takes the journal lock. Force the reverse-order path with
		// a separate orphan whose lifetime lease remains held for all cycles.
		orphan := register(t, store, "cursor")
		held, err := store.AcquireListenerLease(orphan.ID, orphan.Key)
		if err != nil {
			t.Fatal(err)
		}
		defer held.Close(nil)
		if err := os.Remove(sessionFile(store, orphan)); err != nil {
			t.Fatal(err)
		}
		for range 40 {
			leaseLockCycle(t, store, other, a, endpoint)
		}
		return
	}
	// Keep the watchdog outside the lock holders: a regression cannot deadlock
	// the parent's cleanup. This deadline is a safety bound, never a sleep.
	endpoint := listenSocket(t) // Parent owns cleanup even if the watchdog kills the child.
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLeaseOperationsAndPruneDoNotDeadlock$", "-test.count=1", "-test.v")
	cmd.Env = append(os.Environ(), "ZALKI_TEST_LEASE_LOCK_CYCLES=1", "ZALKI_TEST_LEASE_ENDPOINT="+endpoint.Path, "TMPDIR="+t.TempDir())
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("lease/prune/list watchdog: lock-order cycle did not finish within 60s: %v\n%s", ctx.Err(), output)
	}
	if err != nil || strings.Contains(string(output), "--- SKIP:") {
		t.Fatalf("lease/prune/list subprocess did not pass: %v\n%s", err, output)
	}
}

func leaseLockCycle(t *testing.T, store, other bus.Store, a agent, endpoint *model.Endpoint) {
	t.Helper()
	lease, err := store.AcquireListenerLease(a.ID, a.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close(nil)
	done := make(chan error, 2)
	go func() { _, err := other.Prune(false, nil); done <- err }()
	go func() { _, err := other.List(); done <- err }()
	if err := lease.Arm(endpoint); err != nil {
		t.Fatal(err)
	}
	if err := lease.Confirm(func(model.SessionView) (json.RawMessage, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal("prune/list with a held listener lease", err)
		}
	}
}
