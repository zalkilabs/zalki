package bus

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// These tests close gaps found in floodState.admit (flood.go) and
// pruneRun.deleteOrphanFlood (flood_prune.go). They pin today's behavior; odd
// results are characterized, not endorsed.
//
// admit checks in this order:
//  1. A clock that went back is moved forward to the last known time.
//  2. More than floodInboundPerHour recent arrivals: "invalid hourly flood quota state".
//  3. Arrivals are walked in order. Each is checked first (zero time, earlier than
//     the one before, later than the inbound bucket: "invalid flood quota arrival
//     time"), then dropped if at least an hour old, then compared with the message
//     ID: another sender is a conflict, the same sender is an exact retry that
//     returns at once. So a retry hides every arrival after it from the checks.
//  4. Hourly caps: pair, then inbound (rate limited).
//  5. Sender buckets: an invalid one is "invalid sender flood quota state"; an
//     idle one is dropped.
//  6. Short buckets: pair, then inbound (rate limited).
//
// deleteOrphanFlood works in this order: a missing flood directory is fine; a
// directory that is not private (for example a symlink) is refused; then each
// entry must end in .json, be named <kind>_<canonical uuid>.json, and be a
// regular file; a record whose session still exists is kept; a record from a
// newer format is kept; everything else is an orphan and is listed, and removed
// unless this is a dry run. The quota content need not be valid.

// admitWith runs admit on a fresh state that the test has changed.
func admitWith(change func(state *floodState, now time.Time), sender, id string) (floodState, bool, error) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	state := newFloodAdmissionState(now)
	change(&state, now)
	changed, err := state.admit(sender, id, now)
	return state, changed, err
}

func arrivals(now time.Time, count int) []floodArrival {
	list := make([]floodArrival, count)
	for i := range list {
		list[i] = floodArrival{ID: "old-" + string(rune('a'+i%26)) + "-" + time.Duration(i).String(), Sender: "other", At: now.Add(-time.Duration(count-i) * time.Second)}
	}
	return list
}

// admitCase is one invalid or refused state for admit.
type admitCase struct {
	name     string
	change   func(state *floodState, now time.Time)
	id       string
	wantCode ErrorCode
	wantText string
}

const (
	tooManyArrivals = "invalid hourly flood quota state"
	badArrivalTime  = "invalid flood quota arrival time"
	badSenderBucket = "invalid sender flood quota state"
	idConflict      = "message UUID already reserved by another sender"
)

func admitCases() []admitCase {
	zero := floodArrival{ID: "zero", Sender: "other"}
	return []admitCase{
		{"too many recent arrivals", func(s *floodState, now time.Time) { s.Recent = arrivals(now, floodInboundPerHour+1) }, "new", CodeInvalidInput, tooManyArrivals},
		{"an arrival without a time", func(s *floodState, now time.Time) { s.Recent = []floodArrival{zero} }, "new", CodeInvalidInput, badArrivalTime},
		{"arrivals out of order", func(s *floodState, now time.Time) {
			s.Recent = []floodArrival{{ID: "b", Sender: "other", At: now.Add(-time.Second)}, {ID: "a", Sender: "other", At: now.Add(-2 * time.Second)}}
		}, "new", CodeInvalidInput, badArrivalTime},
		{"an arrival after the inbound bucket", func(s *floodState, now time.Time) {
			s.Recent = []floodArrival{{ID: "late", Sender: "other", At: now.Add(time.Second)}}
		}, "new", CodeInvalidInput, badArrivalTime},
		{"a sender bucket without a time", func(s *floodState, now time.Time) { s.Senders["x"] = floodBucket{Tokens: 1} }, "new", CodeInvalidInput, badSenderBucket},
		{"a sender bucket below zero", func(s *floodState, now time.Time) { s.Senders["x"] = floodBucket{Tokens: -1, At: now} }, "new", CodeInvalidInput, badSenderBucket},
		{"a sender bucket over the burst", func(s *floodState, now time.Time) {
			s.Senders["x"] = floodBucket{Tokens: floodPairBurst + 1, At: now}
		}, "new", CodeInvalidInput, badSenderBucket},
		{"a sender bucket that is not a number", func(s *floodState, now time.Time) {
			s.Senders["x"] = floodBucket{Tokens: math.NaN(), At: now}
		}, "new", CodeInvalidInput, badSenderBucket},
		// Two faults at once: the earlier check in the list above wins.
		{"too many arrivals and an arrival without a time", func(s *floodState, now time.Time) {
			s.Recent = append(arrivals(now, floodInboundPerHour), zero)
		}, "new", CodeInvalidInput, tooManyArrivals},
		{"an arrival without a time and a bad sender bucket", func(s *floodState, now time.Time) {
			s.Recent = []floodArrival{zero}
			s.Senders["x"] = floodBucket{Tokens: -1, At: now}
		}, "new", CodeInvalidInput, badArrivalTime},
		{"a time check comes before the age check", func(s *floodState, now time.Time) {
			s.Recent = []floodArrival{{ID: "ancient", Sender: "other", At: now.Add(-48 * time.Hour)}, {ID: "b", Sender: "other", At: now.Add(-49 * time.Hour)}}
		}, "new", CodeInvalidInput, badArrivalTime},
		{"the pair hourly cap comes before a bad sender bucket", func(s *floodState, now time.Time) {
			for i := range floodPairPerHour {
				s.Recent = append(s.Recent, floodArrival{ID: "p" + time.Duration(i).String(), Sender: "sender", At: now.Add(-time.Duration(floodPairPerHour-i) * time.Second)})
			}
			s.Senders["x"] = floodBucket{Tokens: -1, At: now}
		}, "new", CodeRateLimited, "pair hourly cap"},
		// An exact retry or a conflict returns before later arrivals are checked.
		{"a retry hides a bad arrival after it", func(s *floodState, now time.Time) {
			s.Recent = []floodArrival{{ID: "same", Sender: "sender", At: now.Add(-time.Second)}, zero}
		}, "same", "", ""},
		{"a conflict hides a bad arrival after it", func(s *floodState, now time.Time) {
			s.Recent = []floodArrival{{ID: "same", Sender: "other", At: now.Add(-time.Second)}, zero}
		}, "same", CodeConflict, idConflict},
		{"a bad arrival before the retry is still refused", func(s *floodState, now time.Time) {
			s.Recent = []floodArrival{zero, {ID: "same", Sender: "sender", At: now.Add(-time.Second)}}
		}, "same", CodeInvalidInput, badArrivalTime},
	}
}

func TestFloodAdmitRefusesInvalidStateInOrder(t *testing.T) {
	for _, test := range admitCases() {
		t.Run(test.name, func(t *testing.T) {
			_, changed, err := admitWith(test.change, "sender", test.id)
			if changed {
				t.Fatal("an invalid or refused state admitted a message")
			}
			assertAdmitError(t, err, test)
		})
	}
}

// assertAdmitError checks the error of one case. An empty wantText means an
// exact retry: no error and nothing admitted.
func assertAdmitError(t *testing.T, err error, test admitCase) {
	t.Helper()
	if test.wantText == "" {
		if err != nil {
			t.Fatalf("exact retry = %v, want no error", err)
		}
		return
	}
	// A rate limit names the wait time, so only its start is fixed.
	text := string(test.wantCode) + ": " + test.wantText
	matches := err != nil && err.Error() == text
	if test.wantCode == CodeRateLimited {
		matches = err != nil && strings.Contains(err.Error(), test.wantText)
	}
	if !matches || ErrorCodeOf(err) != test.wantCode {
		t.Fatalf("error = %v (code %q), want %q (code %q)", err, ErrorCodeOf(err), test.wantText, test.wantCode)
	}
}

func TestFloodAdmitDoesNotDropAnInvalidSenderBucketAsIdle(t *testing.T) {
	state, _, err := admitWith(func(s *floodState, now time.Time) {
		s.Senders["stale"] = floodBucket{Tokens: -1, At: now.Add(-time.Hour)}
	}, "sender", "new")
	if err == nil || err.Error() != "invalid_input: "+badSenderBucket {
		t.Fatalf("error = %v", err)
	}
	if _, kept := state.Senders["stale"]; !kept {
		t.Fatal("an invalid bucket was dropped instead of refused")
	}
}

// A tampered quota file must refuse the send and stay as it is, the same as
// the partial states in TestFloodPartialOrUnsupportedStateFailsClosed.
func TestFloodTamperedArrivalsAndBucketsFailClosedThroughTheStore(t *testing.T) {
	now := time.Now().UTC()
	valid := func() floodState {
		return floodState{Schema: 1, Target: "", Inbound: floodBucket{Tokens: floodInboundBurst, At: now}, Senders: map[string]floodBucket{}}
	}
	tests := []struct {
		name string
		edit func(state *floodState)
		want string
	}{
		{"too many recent arrivals", func(s *floodState) { s.Recent = arrivals(now, floodInboundPerHour+1) }, "invalid hourly flood quota state"},
		{"an arrival without a time", func(s *floodState) { s.Recent = []floodArrival{{ID: "x", Sender: "y"}} }, "invalid flood quota arrival time"},
		{"a sender bucket below zero", func(s *floodState) { s.Senders["x"] = floodBucket{Tokens: -1, At: now} }, "invalid sender flood quota state"},
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
