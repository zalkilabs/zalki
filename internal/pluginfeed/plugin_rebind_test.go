package pluginfeed

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// U49: a grant or scope change on an enabled plugin is one rebind: the record
// stays enabled with the new access and a fresh epoch, and the old launch no
// longer holds, so its child stops.
func TestPluginGrantEnabledRebind(t *testing.T) {
	for name, change := range map[string]func(PluginFeed) error{
		"grant": func(f PluginFeed) error { return f.GrantPluginAccess(PluginAccess{Status: true, Receive: true}) },
		"scope": func(f PluginFeed) error { return f.ConfigurePluginScope("none") },
	} {
		t.Run(name, func(t *testing.T) {
			s := fixtureStore(t)
			enableFeed(t, s)
			f := NewPluginFeed(s, "fake-notifier")
			launch, err := f.PluginStatus()
			if err != nil {
				t.Fatal(err)
			}
			if err := change(f); err != nil {
				t.Fatal(err)
			}
			after, err := f.PluginStatus()
			if err != nil || !after.Enabled || after.Epoch == launch.Epoch || after.Revision == launch.Revision || !after.GapPending {
				t.Fatalf("not one rebind: %+v %v", after, err)
			}
			if code := f.PluginBindingCode(&launch); code != "stale_binding" {
				t.Fatalf("the old launch still holds: %s", code)
			}
			if code := f.PluginBindingCode(&after); code != "ok" {
				t.Fatalf("the new launch does not hold: %s", code)
			}
		})
	}
}

// U50: a change that cannot be applied leaves the enabled record byte for
// byte: no half state, no broader access, no false success.
func TestPluginGrantRefusedChangeLeavesTheRecord(t *testing.T) {
	s := fixtureStore(t)
	enableFeed(t, s)
	f := NewPluginFeed(s, "fake-notifier")
	before, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func() error{
		"plugin address": func() error { return f.GrantPluginAccess(PluginAccess{Read: []string{"plugin:" + model.NewUUID()}}) },
		"no access":      func() error { return f.GrantPluginAccess(PluginAccess{}) },
		"bad scope":      func() error { return f.ConfigurePluginScope("everything") },
	} {
		if err := change(); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if after, err := os.ReadFile(f.pluginPath()); err != nil || !bytes.Equal(before, after) {
			t.Fatalf("%s: a refused change rewrote the record", name)
		}
	}
}

// U51: remove is one command for an enabled plugin, and a held lease (a
// running host or helper) refuses with nothing removed.
func TestPluginRemoveHeldLease(t *testing.T) {
	s := fixtureStore(t)
	enableFeed(t, s)
	f := NewPluginFeed(s, "fake-notifier")
	lease, err := f.AcquirePluginLease()
	if err != nil {
		t.Fatal(err)
	}
	err = f.RemovePlugin()
	if model.ErrorCodeOf(err) != model.CodeConflict || !strings.Contains(err.Error(), "fake-notifier is running or its lease is held; stop it first, then run zalki plugins remove fake-notifier again") {
		t.Fatal("a held lease did not refuse by name", err)
	}
	if status, err := f.PluginStatus(); err != nil || !status.Enabled {
		t.Fatal("a refused remove changed the installation", status, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatal("remove of an enabled, stopped plugin failed", err)
	}
	if _, err := os.Stat(f.pluginPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the record stayed", err)
	}
}

// U50: a record write is atomic: old or new. A write refused before the
// rename leaves the old record byte for byte and says so; a rename whose
// directory sync fails leaves the new record and says it may be in effect,
// never that nothing changed.
func TestPluginRebindPublicationUncertainty(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission cuts require an ordinary user")
	}
	for name, test := range map[string]struct {
		mode    os.FileMode
		want    string
		changed bool
	}{
		"before rename": {0500, "the access change was not written; fake-notifier keeps its previous access", false},
		"after rename":  {0300, "the access change was written, but the disk did not confirm it; it may be in effect. Check it with: zalki plugins show fake-notifier", true},
	} {
		t.Run(name, func(t *testing.T) { checkPublication(t, test.mode, test.want, test.changed) })
	}
}

// checkPublication cuts the plugin folder's permissions to mode, grants on
// an enabled plugin, and checks the reported outcome and the record bytes.
func checkPublication(t *testing.T, mode os.FileMode, want string, wantChanged bool) {
	t.Helper()
	s := fixtureStore(t)
	enableFeed(t, s)
	f := NewPluginFeed(s, "fake-notifier")
	before, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(f.pluginDir(), 0700) })
	if err := os.Chmod(f.pluginDir(), mode); err != nil {
		t.Fatal(err)
	}
	err = f.GrantPluginAccess(PluginAccess{Status: true, Receive: true})
	if chmodErr := os.Chmod(f.pluginDir(), 0700); chmodErr != nil {
		t.Fatal(chmodErr)
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("publication not reported honestly: %v", err)
	}
	after, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	if changed := !bytes.Equal(before, after); changed != wantChanged {
		t.Fatalf("record changed = %v, want %v", changed, wantChanged)
	}
}

// U50: after a failed write, only what the record proves is reported: the
// new bytes (written, not confirmed), the old bytes (not written), and
// anything else (missing, unreadable, other bytes) as possibly in effect,
// never as unchanged.
func TestPluginPublicationOutcomeIsProvenOrUnknown(t *testing.T) {
	prior, written := []byte(`{"old":1}`), []byte(`{"new":1}`)
	for name, test := range map[string]struct {
		content []byte // nil: the record is missing
		want    string
	}{
		"new image":   {written, "the access change was written, but the disk did not confirm it"},
		"old image":   {prior, "the access change was not written; demo keeps its previous access"},
		"third image": {[]byte(`{"other":1}`), "the access change may or may not be in effect; check it with: zalki plugins show demo"},
		"missing":     {nil, "the access change may or may not be in effect; check it with: zalki plugins show demo"},
		"unreadable":  {prior, "the access change may or may not be in effect; check it with: zalki plugins show demo"},
	} {
		t.Run(name, func(t *testing.T) {
			f := installForTest(t, fixtureStore(t))
			plantRecordImage(t, f, test.content, name == "unreadable")
			err := f.publicationError(prior, written, errors.New("sync failed"))
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "sync failed") {
				t.Fatalf("outcome not reported as proven: %v", err)
			}
		})
	}
}

// plantRecordImage replaces the record with content (nil: removes it) and,
// when unreadable, makes it unreadable: old bytes that cannot be read do not
// count as old.
func plantRecordImage(t *testing.T, f PluginFeed, content []byte, unreadable bool) {
	t.Helper()
	if content == nil {
		if err := os.Remove(f.pluginPath()); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := os.WriteFile(f.pluginPath(), content, 0600); err != nil {
		t.Fatal(err)
	}
	if !unreadable {
		return
	}
	if os.Geteuid() == 0 {
		t.Skip("an unreadable file needs an ordinary user")
	}
	if err := os.Chmod(f.pluginPath(), 0); err != nil {
		t.Fatal(err)
	}
}
