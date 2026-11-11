package pluginfeed

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestNamedPluginIsolationAndReceiptFence(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	foreign, _ := register(t, s, "codex")
	a := installedNamedFeed(t, s, "alpha", v.ID)
	b := installedNamedFeed(t, s, "beta", v.ID)

	assertNamedPluginGrantIsolation(t, s, a, b, v.ID, foreign.ID)
	if err := s.Update(v.ID, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	x, err := a.NextPluginFeed()
	if err != nil || x.Event == nil {
		t.Fatal(x, err)
	}
	y, err := b.NextPluginFeed()
	if err != nil || y.ID != x.ID {
		t.Fatal("independent feeds missed the same event", y, err)
	}
	if err := b.AckPluginFeed(x); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("cross-name receipt accepted", err)
	}
	if err := a.AckPluginFeed(x); err != nil {
		t.Fatal(err)
	}
	if next, err := b.NextPluginFeed(); err != nil || next.ID != y.ID {
		t.Fatal("one plugin advanced another", next, err)
	}
	if err := b.AckPluginFeed(y); err != nil {
		t.Fatal(err)
	}
}

func TestNamedPluginRemovalReaddAndPersistentData(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	f := NewPluginFeed(s, "alpha")
	if err := f.AddPlugin(); err != nil {
		t.Fatal(err)
	}
	before, err := f.PluginStatus()
	if err != nil || before.ID == "" || before.Name != "alpha" || before.Enabled {
		t.Fatal(before, err)
	}
	info, err := os.Stat(before.DataDir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("data directory is not 0700", info, err)
	}
	sentinel := filepath.Join(before.DataDir, "persistent")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	old := prepareNamedPluginRemoval(t, f, v.ID)
	assertNamedPluginPersistentData(t, f, before.ID, sentinel)
	assertNamedPluginLeaseProtectsRemoval(t, f)
	if err := f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.pluginPath(), before.DataDir} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("remove retained record/data", path, err)
		}
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatal("remove retry failed", err)
	}
	if err := f.ConfigurePluginFeed([]string{v.ID}); model.ErrorCodeOf(err) != model.CodeNotFound {
		t.Fatal("configure recreated removed plugin", err)
	}
	assertNamedPluginReaddFences(t, f, before.ID, v.ID, old)
}

func TestNamedPluginRejectsInvalidNamesWithoutState(t *testing.T) {
	for _, name := range []string{"", "Alpha", "../escape", "a/b", "-a", "a_", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		t.Run(name, func(t *testing.T) {
			s := bus.Store{Dir: filepath.Join(t.TempDir(), "absent")}
			f := NewPluginFeed(s, name)
			for _, call := range []func() error{f.AddPlugin, f.RemovePlugin, func() error { return f.ConfigurePluginFeed(nil) }, func() error { return f.EnablePluginFeed(true) }, func() error { _, err := f.NextPluginFeed(); return err }, func() error { return f.AckPluginFeed(PluginDelivery{}) }, func() error { _, err := f.PluginStatus(); return err }} {
				if err := call(); model.ErrorCodeOf(err) != model.CodeInvalidInput {
					t.Fatal("accepted invalid name", err)
				}
			}
			if _, err := os.Lstat(s.Dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("invalid name created state", err)
			}
		})
	}
}

func TestNamedPluginDoesNotAdoptAbandonedDataOrLegacyRecord(t *testing.T) {
	s := fixtureStore(t)
	f := NewPluginFeed(s, "alpha")
	if err := f.AddPlugin(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.pluginPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.pluginDataPath(), "old"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.AddPlugin(); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("adopted abandoned data", err)
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Dir, "plugin.json"), []byte(`{"enabled":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil || status.ID != "" || status.Enabled {
		t.Fatal("read legacy record", status, err)
	}
	if err := f.AddPlugin(); err != nil {
		t.Fatal(err)
	}
}

func assertNamedPluginLeaseProtectsRemoval(t *testing.T, f PluginFeed) {
	t.Helper()
	lease, err := f.AcquirePluginLease()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })
	if err := f.RemovePlugin(); err == nil {
		t.Fatal("removed a leased installation")
	}
	if err := f.AddPlugin(); err == nil {
		t.Fatal("replaced a leased installation")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNamedPluginConfigureDoesNotRecreateRemovedInstallation(t *testing.T) {
	s := fixtureStore(t)
	f := NewPluginFeed(s, "alpha")
	if err := f.AddPlugin(); err != nil {
		t.Fatal(err)
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	if err := f.ConfigurePluginFeed([]string{"codex:" + model.NewUUID()}); model.ErrorCodeOf(err) != model.CodeNotFound {
		t.Fatal("configure recreated removed installation", err)
	}
	if _, err := os.Lstat(f.pluginPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("configure wrote a record after remove", err)
	}
	if _, err := os.Lstat(f.pluginDataPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("configure recreated plugin data", err)
	}
}

func installedNamedFeed(t *testing.T, s bus.Store, name string, ids ...string) PluginFeed {
	t.Helper()
	feed := NewPluginFeed(s, name)
	if err := feed.AddPlugin(); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginFeed(ids); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	gap, err := feed.NextPluginFeed()
	if err != nil || gap.Gap != "enabled" {
		t.Fatal(gap, err)
	}
	if err := feed.AckPluginFeed(gap); err != nil {
		t.Fatal(err)
	}
	return feed
}

func assertNamedPluginGrantIsolation(t *testing.T, s bus.Store, a, b PluginFeed, id, foreign string) {
	t.Helper()
	if err := a.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := a.ConfigurePluginFeed([]string{id, foreign}); err != nil {
		t.Fatal(err)
	}
	if err := a.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	gap, err := a.NextPluginFeed()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.AckPluginFeed(gap); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(foreign, model.StateBusy, false); err != nil {
		t.Fatal(err)
	}
	own, err := a.NextPluginFeed()
	if err != nil || own.Event == nil || own.Event.Session.ID != foreign {
		t.Fatal("per-name grants were lost", own, err)
	}
	if err := a.AckPluginFeed(own); err != nil {
		t.Fatal(err)
	}
	if skipped, err := b.NextPluginFeed(); err != nil || skipped.ID != "" {
		t.Fatal("one plugin exposed another's grant", skipped, err)
	}

}

func assertNamedPluginReaddFences(t *testing.T, f PluginFeed, previousID, id string, old PluginDelivery) {
	t.Helper()
	if err := f.AddPlugin(); err != nil {
		t.Fatal(err)
	}
	after, err := f.PluginStatus()
	if err != nil || after.ID == previousID || after.Enabled {
		t.Fatal("re-add reused identity or enabled state", after, err)
	}
	if err := f.ConfigurePluginFeed([]string{id}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if err := f.AckPluginFeed(old); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("receipt crossed re-add", err)
	}
}

func assertNamedPluginPersistentData(t *testing.T, f PluginFeed, id, sentinel string) {
	t.Helper()
	current, err := f.PluginStatus()
	if err != nil || current.ID != id {
		t.Fatal("identity changed", current, err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatal("data did not persist", err)
	}
}

func prepareNamedPluginRemoval(t *testing.T, f PluginFeed, id string) PluginDelivery {
	t.Helper()
	if err := f.ConfigurePluginFeed([]string{id}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	old, err := f.NextPluginFeed()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.RemovePlugin(); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("removed enabled plugin", err)
	}
	if err := f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	return old
}
