package install

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const relaunchTarget = "/usr/local/bin/zalki"

// plantRelaunch installs the item on the fixture's launcher (B1) and leaves
// a prepared relaunch to relaunchTarget (B2), as a crash right after step 1
// would. It returns the desired record.
func plantRelaunch(t *testing.T, f *loginFixture) loginItemRecord {
	t.Helper()
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	var prior loginItemRecord
	if _, err := readPrivateJSON(f.paths.record, &prior); err != nil {
		t.Fatal(err)
	}
	desired, err := desiredLoginItem(f.paths, relaunchTarget, &prior)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateJSON(f.paths.pending, loginItemPending{Schema: 1, Operation: strings.Repeat("f", 32), Action: "relaunch", Phase: "prepared", Prior: &prior, Desired: desired}); err != nil {
		t.Fatal(err)
	}
	return *desired
}

// checkRelaunched asserts the finished state: the B2 record, the exact B2
// plist (0600), no pending operation, and a bootstrap after any bootout.
func checkRelaunched(t *testing.T, f *loginFixture, name string, desired loginItemRecord) {
	t.Helper()
	var record loginItemRecord
	if _, err := readPrivateJSON(f.paths.record, &record); err != nil || record != desired {
		t.Fatalf("%s: the B2 record was not committed: %+v %v", name, record, err)
	}
	info, err := os.Lstat(f.paths.plist)
	raw, _ := os.ReadFile(f.paths.plist)
	if err != nil || info.Mode().Perm() != 0o600 || sha256Hex(raw) != desired.PlistSHA {
		t.Fatalf("%s: the B2 plist is not the exact private file: %v %v", name, info.Mode(), err)
	}
	if _, err := os.Lstat(f.paths.pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s: the relaunch stayed pending", name)
	}
	calls := f.fake.mutations()
	if i, j := slices.Index(calls, "bootout"), slices.Index(calls, "bootstrap"); j < 0 || (i >= 0 && i > j) {
		t.Fatalf("%s: launchd calls out of order: %v", name, calls)
	}
}

// A relaunch cut at any step is finished by an install for its own
// launcher, with no new question, from what is observed now.
func TestPluginLoginItemRelaunchCrashPoints(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cut   func(f *loginFixture, desired loginItemRecord)
		wants string // the launchd change that must still run, besides bootstrap
	}{
		{"after pending, before bootout", func(*loginFixture, loginItemRecord) {}, "bootout"},
		{"after bootout, before confirm", func(f *loginFixture, _ loginItemRecord) { f.fake.loaded = false }, ""},
		{"temp written, not renamed", func(f *loginFixture, _ loginItemRecord) {
			f.fake.loaded = false
			writeFile(t, filepath.Join(filepath.Dir(f.paths.plist), ".zalki-config-123"), []byte("partial"))
		}, ""},
		{"renamed, before commit", func(f *loginFixture, d loginItemRecord) {
			f.fake.loaded = false
			writeFile(t, f.paths.plist, renderLoginItem(d.Binary, d.State))
		}, ""},
		{"committed, before pending removal", func(f *loginFixture, d loginItemRecord) {
			f.fake.loaded = false
			writeFile(t, f.paths.plist, renderLoginItem(d.Binary, d.State))
			if err := writePrivateJSON(f.paths.record, d); err != nil {
				t.Fatal(err)
			}
		}, ""},
	} {
		f := newLoginFixture(t)
		desired := plantRelaunch(t, f)
		tc.cut(f, desired)
		f.binary, f.fake.calls = relaunchTarget, nil
		f.fake.program, f.fake.args = relaunchTarget, []string{relaunchTarget, "plugins", "supervise", "--state", f.state}
		if tc.wants == "bootout" { // B1 is still the loaded job
			f.fake.program, f.fake.args = "/opt/homebrew/bin/zalki", []string{"/opt/homebrew/bin/zalki", "plugins", "supervise", "--state", f.state}
			f.fake.onMutate = func(verb string) {
				if verb == "bootout" {
					f.fake.program, f.fake.args = relaunchTarget, []string{relaunchTarget, "plugins", "supervise", "--state", f.state}
				}
			}
		}
		if _, err := f.do("install", true); err != nil {
			t.Fatalf("%s: the resumed relaunch failed: %v", tc.name, err)
		}
		if tc.wants == "bootout" && !slices.Contains(f.fake.mutations(), "bootout") {
			t.Fatalf("%s: the B1 job was not stopped", tc.name)
		}
		checkRelaunched(t, f, tc.name, desired)
	}
}

// A bootout that fails (or a job that stays loaded) keeps the relaunch
// pending and writes no B2 plist.
func TestPluginLoginItemRelaunchStopMustBeConfirmed(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(f.paths.plist)
	f.fake.bootoutFails = true
	f.binary, f.fake.calls = relaunchTarget, nil
	_, err := f.do("relaunch", true)
	after, _ := os.ReadFile(f.paths.plist)
	_, pendingErr := os.Lstat(f.paths.pending)
	if err == nil || pendingErr != nil || string(after) != string(before) || slices.Contains(f.fake.mutations(), "bootstrap") {
		t.Fatalf("an unconfirmed stop let the relaunch go on: %v %v", err, f.fake.mutations())
	}

	// launchd answers the bootout with success, but the job is still loaded.
	g := newLoginFixture(t)
	if _, err := g.do("install", true); err != nil {
		t.Fatal(err)
	}
	before, _ = os.ReadFile(g.paths.plist)
	g.fake.onMutate = func(verb string) {
		if verb == "bootout" {
			g.fake.onPrint = func() { g.fake.loaded = true }
		}
	}
	g.binary, g.fake.calls = relaunchTarget, nil
	_, err = g.do("relaunch", true)
	after, _ = os.ReadFile(g.paths.plist)
	_, pendingErr = os.Lstat(g.paths.pending)
	if err == nil || pendingErr != nil || string(after) != string(before) || slices.Contains(g.fake.mutations(), "bootstrap") {
		t.Fatalf("an unconfirmed stop let the relaunch go on: %v %v", err, g.fake.mutations())
	}
}

// A known bootstrap failure after the commit keeps the B2 record (the
// change is Zalki's) and reports the failure; nothing stays pending.
func TestPluginLoginItemRelaunchBootstrapFailure(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	f.binary, f.fake.bootstrapFails = relaunchTarget, true
	_, err := f.do("relaunch", true)
	var record loginItemRecord
	_, readErr := readPrivateJSON(f.paths.record, &record)
	_, pendingErr := os.Lstat(f.paths.pending)
	if err == nil || readErr != nil || record.Binary != relaunchTarget || !errors.Is(pendingErr, os.ErrNotExist) {
		t.Fatalf("a failed bootstrap after the commit lost the B2 record: %v %+v %v", err, record, pendingErr)
	}
}

// B3: an unfinished relaunch to B2 is never finished toward another launcher;
// the owner gets one line and nothing changes.
func TestPluginLoginItemRelaunchOtherLauncher(t *testing.T) {
	f := newLoginFixture(t)
	plantRelaunch(t, f)
	f.binary, f.fake.calls = "/opt/zalki/bin/zalki", nil
	before := snapshotFiles(t, f.paths)
	_, err := f.do("install", true)
	want := "The zalki login item is being changed to " + relaunchTarget + ", not /opt/zalki/bin/zalki; nothing was changed."
	if err == nil || err.Error() != want || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
		t.Fatalf("a relaunch was finished toward another launcher: %v %v", err, f.fake.mutations())
	}
}

// A relaunch never starts over a job that is not ours, and a plist that is
// neither B1's nor B2's bytes is kept.
func TestPluginLoginItemRelaunchRefusesForeign(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	f.fake.program, f.fake.calls = "/tmp/other", nil
	f.binary = relaunchTarget
	before := snapshotFiles(t, f.paths)
	if _, err := f.do("relaunch", true); !errors.Is(err, errLoginItemLoaded) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
		t.Fatalf("a relaunch started over a job that is not ours: %v %v", err, f.fake.mutations())
	}

	g := newLoginFixture(t)
	plantRelaunch(t, g)
	writeFile(t, g.paths.plist, []byte("changed"))
	g.binary, g.fake.calls = relaunchTarget, nil
	if _, err := g.do("install", true); err == nil || len(g.fake.mutations()) != 0 {
		t.Fatalf("a relaunch went on over changed plist bytes: %v %v", err, g.fake.mutations())
	}
}

// Inspect names the launcher only from a fully validated owned record with no
// unfinished operation; every other state leaves it empty.
func TestPluginLoginItemInspectNamesLauncher(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *loginFixture)
		want  func(f *loginFixture) string
	}{
		{"owned valid", func(f *loginFixture) {
			if _, err := f.do("install", true); err != nil {
				t.Fatal(err)
			}
		}, func(f *loginFixture) string { return f.binary }},
		{"damaged record", func(f *loginFixture) {
			if _, err := f.do("install", true); err != nil {
				t.Fatal(err)
			}
			writeFile(t, f.paths.record, []byte("{"))
		}, func(*loginFixture) string { return "" }},
		{"foreign plist", func(f *loginFixture) { writeFile(t, f.paths.plist, renderLoginItem(f.binary, f.state)) }, func(*loginFixture) string { return "" }},
		{"pending only", func(f *loginFixture) { plantOwnSetup(t, f, "pending only") }, func(*loginFixture) string { return "" }},
		{"none", func(*loginFixture) {}, func(*loginFixture) string { return "" }},
		{"changed plist", func(f *loginFixture) {
			if _, err := f.do("install", true); err != nil {
				t.Fatal(err)
			}
			writeFile(t, f.paths.plist, []byte("changed"))
		}, func(*loginFixture) string { return "" }},
		{"pending relaunch", func(f *loginFixture) { plantRelaunch(t, f) }, func(*loginFixture) string { return "" }},
	} {
		f := newLoginFixture(t)
		tc.setup(f)
		if report, _ := f.do("inspect", false); report.LoginItemLauncher != tc.want(f) {
			t.Errorf("%s: inspect named launcher %q, want %q", tc.name, report.LoginItemLauncher, tc.want(f))
		}
	}
}

// A B2 plist that cannot be written keeps the B1 record committed: the
// record never names bytes that are not durable on disk.
func TestPluginLoginItemRelaunchPlistWriteFailureKeepsRecord(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	var prior loginItemRecord
	if _, err := readPrivateJSON(f.paths.record, &prior); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(f.paths.plist)
	f.binary, f.fake.calls = relaunchTarget, nil
	f.fake.onMutate = func(verb string) {
		if verb == "bootout" {
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Error(err)
			}
		}
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	_, err := f.do("relaunch", true)
	_ = os.Chmod(dir, 0o700)
	var record loginItemRecord
	_, readErr := readPrivateJSON(f.paths.record, &record)
	if err == nil || readErr != nil || record != prior || slices.Contains(f.fake.mutations(), "bootstrap") {
		t.Fatalf("a failed B2 plist write committed the B2 record: %v %+v %v", err, record, f.fake.mutations())
	}
}

// The relaunch consent is bound to the record the owner was shown: while
// the question waits, another authorized relaunch B1 -> B3 finishes; the
// first caller (shown B1) is refused under the lock with zero changes, and
// B3 keeps running. A relaunch without a binding is refused too, and an
// install shown "no item" never acts on an item that appeared meanwhile.
func TestPluginLoginItemConsentBoundToShownRecord(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	shown, _ := f.do("inspect", false) // the question shows B1
	const b3 = "/opt/zalki/bin/zalki"
	f.binary = b3 // the other authorized change, while the question waits
	f.fake.onMutate = func(verb string) {
		if verb == "bootout" {
			f.fake.program, f.fake.args = b3, []string{b3, "plugins", "supervise", "--state", f.state}
		}
	}
	if _, err := f.do("relaunch", true); err != nil {
		t.Fatal(err)
	}
	f.binary, f.fake.calls = relaunchTarget, nil
	before := snapshotFiles(t, f.paths)
	if _, err := f.doExpect("relaunch", shown.LoginItemRecord); !errors.Is(err, errLoginItemChanged) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
		t.Fatalf("a relaunch shown B1 acted on the B3 item: %v %v", err, f.fake.mutations())
	}
	if _, err := f.doExpect("relaunch", ""); !errors.Is(err, errLoginItemChanged) || len(f.fake.mutations()) != 0 {
		t.Fatalf("a relaunch without a binding was accepted: %v %v", err, f.fake.mutations())
	}
	// A relaunch to the launcher the item already runs starts nothing, so it
	// is refused: "Start requested" is never reported for a no-op.
	current, _ := f.do("inspect", false)
	f.binary = b3
	if _, err := f.doExpect("relaunch", current.LoginItemRecord); !errors.Is(err, errLoginItemChanged) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
		t.Fatalf("a relaunch to the same launcher reported success: %v %v", err, f.fake.mutations())
	}

	checkInstallShownNone(t)
}

// checkInstallShownNone: an install shown "no item" never acts on an item
// that was set up meanwhile.
func checkInstallShownNone(t *testing.T) {
	t.Helper()
	g := newLoginFixture(t)
	none, _ := g.do("inspect", false)
	if _, err := g.do("install", true); err != nil { // set up meanwhile
		t.Fatal(err)
	}
	g.fake.calls = nil
	before := snapshotFiles(t, g.paths)
	if _, err := g.doExpect("install", none.LoginItemRecord); none.LoginItemRecord != "none" || !errors.Is(err, errLoginItemChanged) || len(g.fake.mutations()) != 0 || snapshotFiles(t, g.paths) != before {
		t.Fatalf("an install shown no item acted on a new one: %q %v %v", none.LoginItemRecord, err, g.fake.mutations())
	}
}
