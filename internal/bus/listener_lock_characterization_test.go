package bus_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func listenerLockFile(store bus.Store, a agent) string {
	return filepath.Join(store.Dir, a.Kind+"-listener-"+a.NativeID+".lock")
}

func keysOf(record map[string]any) []string {
	names := make([]string, 0, len(record))
	for name := range record {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestListenerLeaseLifecycleAsPersisted pins the lease as the journal records it:
// the record gains a "listener" object under the newer session schema while
// armed (so older builds refuse to overwrite it), and returns to the older
// schema without it once the lease closes.
func TestListenerLeaseLifecycleAsPersisted(t *testing.T) {
	store := newStore(t, listenerAdapter("cursor"))
	a := register(t, store, "cursor")
	path := sessionFile(store, a)
	if record := readJSONFile(t, path); record["schema"] != float64(1) || record["listener"] != nil {
		t.Fatalf("fresh registration: schema %v listener %v", record["schema"], record["listener"])
	}

	lease, err := store.AcquireListenerLease(a.ID, a.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close(nil)
	if lease.Generation() != a.Generation {
		t.Fatalf("lease generation %q, want the registration's %q", lease.Generation(), a.Generation)
	}
	if _, err := os.Stat(listenerLockFile(store, a)); err != nil {
		t.Fatalf("acquiring a lease must create its lifetime lock file: %v", err)
	}
	if record := readJSONFile(t, path); record["listener"] != nil {
		t.Fatal("acquiring a lease must not record it before it is armed")
	}

	endpoint := listenSocket(t)
	if err := lease.Arm(endpoint); err != nil {
		t.Fatal(err)
	}
	armed := readJSONFile(t, path)
	listener, _ := armed["listener"].(map[string]any)
	if armed["schema"] != float64(2) || strings.Join(keysOf(listener), ",") != "endpoint,generation,id" || listener["generation"] != a.Generation {
		t.Fatalf("armed record: schema %v listener %v", armed["schema"], listener)
	}
	if got := strings.Join(keysOf(listener["endpoint"].(map[string]any)), ","); got != "changed_ns,device,inode,path" {
		t.Fatalf("endpoint keys = %s", got)
	}

	if err := lease.Confirm(func(model.SessionView) (json.RawMessage, error) { return nil, nil }); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if record := readJSONFile(t, path); record["listener"] == nil {
		t.Fatal("confirming a lease must keep it recorded")
	}

	lease.Close(nil)
	closed := readJSONFile(t, path)
	if closed["schema"] != float64(1) || closed["listener"] != nil {
		t.Fatalf("closed record: schema %v listener %v", closed["schema"], closed["listener"])
	}
}

// pruneInSubprocess runs a real second process against the same journal. If a
// regression makes prune block on the parent's lifetime lock while holding the
// journal lock, the child can be killed before parent lease cleanup runs.
func pruneInSubprocess(t *testing.T, store bus.Store, removed string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestPruneKeepsAnOrphanedListenerLockWhileItIsHeld$", "-test.count=1")
	cmd.Env = append(os.Environ(), "ZALKI_CHARACTERIZATION_PRUNE_STATE="+store.Dir, "ZALKI_CHARACTERIZATION_REMOVED_LOCK="+removed)
	cmd.WaitDelay = time.Second
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("Prune exceeded its subprocess deadline: possible blocking lifetime-lock acquisition: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("Prune subprocess: %v\n%s", err, output)
	}
}

// TestPruneKeepsAnOrphanedListenerLockWhileItIsHeld exercises the actual
// reversed lock path: an orphan makes prune attempt the lifetime lock while
// holding the journal lock. It must return while another process holds that
// lifetime lock, and may remove the orphan only after its holder releases it.
func TestPruneKeepsAnOrphanedListenerLockWhileItIsHeld(t *testing.T) {
	if dir := os.Getenv("ZALKI_CHARACTERIZATION_PRUNE_STATE"); dir != "" {
		store := newStore(t, listenerAdapter("cursor"))
		store.Dir = dir
		report, err := store.Prune(false, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := strings.Join(report.DeletedLocks, ","), os.Getenv("ZALKI_CHARACTERIZATION_REMOVED_LOCK"); got != want {
			t.Fatalf("Prune deleted locks %q, want %q", got, want)
		}
		return
	}

	store := newStore(t, listenerAdapter("cursor"))
	a := register(t, store, "cursor")
	lease, err := store.AcquireListenerLease(a.ID, a.Key)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close(nil)
	lock := listenerLockFile(store, a)

	// With its session present, the lock belongs to a live registration.
	pruneInSubprocess(t, store, "")

	// Without its session the lock is an orphan, but this one is still held.
	if err := os.Remove(sessionFile(store, a)); err != nil {
		t.Fatal(err)
	}
	pruneInSubprocess(t, store, "")
	if _, err := os.Stat(lock); err != nil {
		t.Fatalf("Prune removed a lifetime lock that is held: %v", err)
	}
	if second, err := store.AcquireListenerLease(a.ID, a.Key); err == nil {
		second.Close(nil)
		t.Fatal("a second lease was acquired for a session that no longer exists")
	} else if bus.ErrorCodeOf(err) != "not_found" {
		t.Fatalf("missing session: error code %q (%v), want not_found", bus.ErrorCodeOf(err), err)
	}

	// Once the holder lets go, the orphan is removed.
	lease.Close(nil)
	pruneInSubprocess(t, store, filepath.Base(lock))
	if _, err := os.Stat(lock); !os.IsNotExist(err) {
		t.Fatalf("a free orphan lock remained: %v", err)
	}
}
