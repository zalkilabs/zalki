package bus

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// approvalHolds counts the messages that wait on each target, leaves out the
// legacy approval refusals, and lists every message file it cannot read by ID,
// in ID order. An unreadable file adds to no count and does not stop the scan.
func TestApprovalHoldsCountsPerTargetAndListsEveryUnreadableID(t *testing.T) {
	store := fixtureStore(t)
	first, _, _ := queueThenDropCount(t, store)
	second, _, _ := queueThenDropCount(t, store)
	broken := []string{newID(), newID(), newID()}
	for _, id := range broken {
		if err := os.WriteFile(filepath.Join(store.dir, "messages", id+".json"), []byte("{bad"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	slices.Sort(broken)
	var counts map[string]int
	var unreadable []string
	if err := store.locked(func() (err error) {
		counts, unreadable, err = approvals(store).approvalHolds()
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := map[string]int{first.ID: 1, second.ID: 1}; !maps.Equal(counts, want) {
		t.Fatalf("counts = %v, want %v", counts, want)
	}
	if !slices.Equal(unreadable, broken) {
		t.Fatalf("unreadable = %v, want %v", unreadable, broken)
	}
}
