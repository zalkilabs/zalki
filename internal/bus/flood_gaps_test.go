package bus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// These Store/prune tests retain fail-closed quota and orphan cleanup behavior.

func arrivals(now time.Time, count int) []model.FloodArrival {
	list := make([]model.FloodArrival, count)
	for i := range list {
		list[i] = model.FloodArrival{ID: "old-" + string(rune('a'+i%26)) + "-" + time.Duration(i).String(), Sender: "other", At: now.Add(-time.Duration(count-i) * time.Second)}
	}
	return list
}

// A tampered quota file must refuse the send and stay as it is, the same as
// the partial states in TestFloodPartialOrUnsupportedStateFailsClosed.
func TestFloodTamperedArrivalsAndBucketsFailClosedThroughTheStore(t *testing.T) {
	now := time.Now().UTC()
	valid := func() floodRecord {
		return withFloodQuota(floodRecord{Schema: 1, Target: ""}, model.NewFloodState(now))
	}
	tests := []struct {
		name string
		edit func(state *floodRecord)
		want string
	}{
		{"too many recent arrivals", func(s *floodRecord) { s.Recent = arrivals(now, 400+1) }, "invalid hourly flood quota state"},
		{"an arrival without a time", func(s *floodRecord) { s.Recent = []model.FloodArrival{{ID: "x", Sender: "y"}} }, "invalid flood quota arrival time"},
		{"a sender bucket below zero", func(s *floodRecord) { s.Senders["x"] = model.FloodBucket{Tokens: -1, At: now} }, "invalid sender flood quota state"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := fixtureStore(t)
			msg := floodMessage()
			if err := spendFlood(store, msg, now); err != nil {
				t.Fatal(err)
			}
			state := valid()
			state.Target = msg.To
			test.edit(&state)
			data, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			path, _ := store.floodPath(msg.To)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			msg.ID = newID()
			err = spendFlood(store, msg, now)
			if err == nil || ErrorCodeOf(err) != CodeInvalidInput || err.Error() != "invalid_input: "+test.want {
				t.Fatalf("error = %v (code %q), want %q", err, ErrorCodeOf(err), test.want)
			}
			if string(floodBytes(t, store, msg.To)) != string(data) {
				t.Fatal("an invalid quota state was overwritten")
			}
		})
	}
}

func TestPruneFloodKeepsFutureRecordsAndNonJSONFilesAndListsTheRest(t *testing.T) {
	store := fixtureStore(t)
	dir := filepath.Join(store.Dir, "flood")
	if err := store.locked(func() error { return privateDir(dir) }); err != nil {
		t.Fatal(err)
	}
	write := func(base, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, base), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Orphans: no session exists for any of these names.
	orphanOld := "cursor_" + newID() + ".json"
	orphanBroken := "codex_" + newID() + ".json"
	orphanCurrent := "cursor_" + newID() + ".json"
	write(orphanOld, `{"schema":1}`)
	write(orphanBroken, `not json at all`)
	write(orphanCurrent, `{"schema":`+jsonNumber(recordSchema)+`}`)
	// Kept: a record from a newer build, and files that are not quota records.
	future := "cursor_" + newID() + ".json"
	write(future, `{"schema":`+jsonNumber(recordSchema+1)+`,"extra":"kept"}`)
	// The bare name is a valid <kind>_<uuid>; only the missing .json keeps it.
	// An uppercase UUID is valid input to UUID(), but not a canonical filename.
	upper := "cursor_" + strings.ToUpper("0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d") + ".json"
	notRecords := []string{"cursor_" + newID(), "cursor_" + newID() + ".json.bak", "cursor_" + newID() + ".txt", "README", upper}
	for _, base := range notRecords {
		write(base, "keep")
	}
	kept := append([]string{future}, notRecords...)
	want := []string{orphanOld, orphanBroken, orphanCurrent}
	slices.Sort(want)

	preview, err := store.Prune(true, nil)
	if err != nil || !slices.Equal(preview.DeletedFlood, want) {
		t.Fatalf("dry run listed %v (%v), want %v", preview.DeletedFlood, err, want)
	}
	for _, base := range append(slices.Clone(want), kept...) {
		if _, err := os.Lstat(filepath.Join(dir, base)); err != nil {
			t.Fatalf("a dry run removed %s: %v", base, err)
		}
	}
	report, err := store.Prune(false, nil)
	if err != nil || !slices.Equal(report.DeletedFlood, want) {
		t.Fatalf("applied prune listed %v (%v), want the dry-run list %v", report.DeletedFlood, err, want)
	}
	for _, base := range want {
		if _, err := os.Lstat(filepath.Join(dir, base)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s remained: %v", base, err)
		}
	}
	for _, base := range kept {
		if content, err := os.ReadFile(filepath.Join(dir, base)); err != nil || len(content) == 0 {
			t.Fatalf("prune removed or emptied %s: %v", base, err)
		}
	}
}

func jsonNumber(n int) string {
	data, _ := json.Marshal(n)
	return string(data)
}

// makeSymlinkedFloodDir makes dir a symlink to a private directory elsewhere.
func makeSymlinkedFloodDir(t *testing.T, dir string) {
	t.Helper()
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, dir); err != nil {
		t.Fatal(err)
	}
}

// makeGroupOpenFloodDir makes dir a real directory that the group can read.
func makeGroupOpenFloodDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0750); err != nil {
		t.Fatal(err)
	}
}

func TestPruneFloodRefusesADirectoryThatIsNotPrivate(t *testing.T) {
	setups := map[string]func(t *testing.T, dir string){
		"a symlink to a directory":      makeSymlinkedFloodDir,
		"a directory open to the group": makeGroupOpenFloodDir,
	}
	for name, setup := range setups {
		t.Run(name, func(t *testing.T) {
			store := fixtureStore(t)
			if err := store.locked(func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(store.Dir, "flood")
			setup(t, dir)
			orphan := "cursor_" + newID() + ".json"
			if err := os.WriteFile(filepath.Join(dir, orphan), []byte(`{"schema":1}`), 0600); err != nil {
				t.Fatal(err)
			}
			for _, dry := range []bool{true, false} {
				assertPruneRefusesFloodDir(t, store, dir, orphan, dry)
			}
		})
	}
}

// assertPruneRefusesFloodDir checks that a prune refuses the flood directory
// and leaves the orphan record in place.
func assertPruneRefusesFloodDir(t *testing.T, store Store, dir, orphan string, dry bool) {
	t.Helper()
	report, err := store.Prune(dry, nil)
	if err == nil || !strings.Contains(err.Error(), "state directory must be a private directory, not a symlink: "+dir) {
		t.Fatalf("dry=%v: error = %v, want a private-directory refusal", dry, err)
	}
	if len(report.DeletedFlood) != 0 {
		t.Fatalf("dry=%v: refused prune listed %v", dry, report.DeletedFlood)
	}
	if _, err := os.Lstat(filepath.Join(dir, orphan)); err != nil {
		t.Fatalf("dry=%v: a refused prune removed the record: %v", dry, err)
	}
}
