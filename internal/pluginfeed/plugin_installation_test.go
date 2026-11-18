package pluginfeed

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamNamedIsolationRequiresOwnAttempt(t *testing.T) {
	store := fixtureStore(t)
	native, _ := register(t, store, "codex")
	a, sa := currentNamedStream(t, store, "alpha")
	b, sb := currentNamedStream(t, store, "beta")
	primeNamedStreamViews(t, store, native.ID, []streamTestBinding{{a, sa}, {b, sb}})
	updateStreamState(t, store, native.ID, model.StateBusy)
	ae := streamInspect(t, a, sa)
	be := streamInspect(t, b, sb)
	if ae.ID == "" || ae.ID != be.ID {
		t.Fatal("same immutable event must be prepared in both views")
	}
	streamAttemptAll(t, a, sa, ae)
	assertNamedPreparedAckRefused(t, b, sa, sb, ae.ID)
	acknowledgeStream(t, a, sa, ae.ID)
	if again := streamInspect(t, b, sb); again.ID != be.ID {
		t.Fatal("F_NAMED_A_ACK_NEVER_ADVANCES_B")
	}
	streamAttemptAll(t, b, sb, be)
	if _, err := b.AckPluginFeed(sb, be.ID); err != nil {
		t.Fatal("F_NAMED_B_OWN_ATTEMPT_POSITIVE", err)
	}
	ac, bc := readStreamCheckpoint(t, a), readStreamCheckpoint(t, b)
	if ac.Cursor != bc.Cursor || ac.LastAck != ae.ID || bc.LastAck != be.ID {
		t.Fatal("independent own ACK progress")
	}
	if err := b.ConfigurePluginScope("none"); err != nil {
		t.Fatal(err)
	}
	sb = streamTestLaunch(t, b)
	streamReady(t, b, sb)
	updateStreamState(t, store, native.ID, model.StateReady)
	_, fact := streamTake(t, a, sa)
	if fact.Event == nil || fact.Event.Session.ID != native.ID {
		t.Fatal("per-name allowed scope lost its native fact")
	}
	if denied := streamInspect(t, b, sb); denied.ID != "" {
		t.Fatal("scope none installation observed native fact")
	}
}

func assertNamedPreparedAckRefused(t *testing.T, feed PluginFeed, foreign, own *PluginFeedStatus, id string) {
	t.Helper()
	before := readCommitFile(t, feed.streamPath())
	if _, err := feed.AckPluginFeed(own, id); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("F_NAMED_FOREIGN_ATTEMPT_CANNOT_AUTHORIZE_PREPARED_B", err)
	}
	if _, err := feed.AckPluginFeed(foreign, id); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("F_NAMED_FOREIGN_CAPTURED_LAUNCH_REFUSED", err)
	}
	if !bytes.Equal(before, readCommitFile(t, feed.streamPath())) {
		t.Fatal("F_NAMED_REFUSED_ACK_B_CHECKPOINT_UNCHANGED")
	}
}

type streamTestBinding struct {
	feed   PluginFeed
	launch *PluginFeedStatus
}

func primeNamedStreamViews(t *testing.T, store bus.Store, id string, bindings []streamTestBinding) {
	t.Helper()
	updateStreamState(t, store, id, model.StateBusy)
	for _, binding := range bindings {
		if first := streamInspect(t, binding.feed, binding.launch); first.ID != "" {
			t.Fatal("B_NAMED_FIRST_SEEN_PRIVATE_FLOOR")
		}
	}
	updateStreamState(t, store, id, model.StateReady)
	for _, binding := range bindings {
		offer, frame := streamTake(t, binding.feed, binding.launch)
		if frame.Event == nil || frame.Event.Session.ID != id {
			t.Fatal("B_NAMED_OWN_POST_FLOOR_POSITIVE")
		}
		acknowledgeStream(t, binding.feed, binding.launch, offer.ID)
	}
}

func currentNamedStream(t *testing.T, store bus.Store, name string) (PluginFeed, *PluginFeedStatus) {
	t.Helper()
	feed := NewPluginFeed(store, name)
	if err := feed.AddExternalPlugin(testExecutable(t), nil); err != nil {
		t.Fatal(err)
	}
	if err := feed.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := feed.GrantPluginAccess(PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := feed.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	launch := streamTestLaunch(t, feed)
	streamReady(t, feed, launch)
	return feed, launch
}

func TestNamedPluginRemovalReaddAndPersistentData(t *testing.T) {
	s := fixtureStore(t)
	register(t, s, "codex")
	f := NewPluginFeed(s, "alpha")
	if err := f.AddExternalPlugin(testExecutable(t), nil); err != nil {
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
	old := prepareNamedPluginRemoval(t, f)
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
	if err := f.ConfigurePluginScope("all"); model.ErrorCodeOf(err) != model.CodeNotFound {
		t.Fatal("configure recreated removed plugin", err)
	}
	assertNamedPluginReaddFences(t, f, before.ID, old)
}

func TestNamedPluginRejectsInvalidNamesWithoutState(t *testing.T) {
	for _, name := range []string{"", "Alpha", "../escape", "a/b", "-a", "a_", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"} {
		t.Run(name, func(t *testing.T) {
			s := bus.Store{Dir: filepath.Join(t.TempDir(), "absent")}
			f := NewPluginFeed(s, name)
			for _, call := range []func() error{func() error { return f.AddExternalPlugin("/bin/sh", nil) }, f.RemovePlugin, func() error { return f.ConfigurePluginScope("all") }, func() error { return f.EnablePluginFeed(true) }, func() error { _, err := nextFeed(f); return err }, func() error { return testFeedAck(f, PluginDelivery{}) }, func() error { _, err := f.PluginStatus(); return err }} {
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
	if err := f.AddExternalPlugin(testExecutable(t), nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.pluginPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.pluginDataPath(), "old"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := f.AddExternalPlugin(testExecutable(t), nil); model.ErrorCodeOf(err) != model.CodeConflict {
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
	if err := f.AddExternalPlugin(testExecutable(t), nil); err != nil {
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
	if err := f.AddExternalPlugin(testExecutable(t), nil); err == nil {
		t.Fatal("replaced a leased installation")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestNamedPluginConfigureDoesNotRecreateRemovedInstallation(t *testing.T) {
	s := fixtureStore(t)
	f := NewPluginFeed(s, "alpha")
	if err := f.AddExternalPlugin(testExecutable(t), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	if err := f.ConfigurePluginScope("all"); model.ErrorCodeOf(err) != model.CodeNotFound {
		t.Fatal("configure recreated removed installation", err)
	}
	if _, err := os.Lstat(f.pluginPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("configure wrote a record after remove", err)
	}
	if _, err := os.Lstat(f.pluginDataPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("configure recreated plugin data", err)
	}
}

func assertNamedPluginReaddFences(t *testing.T, f PluginFeed, previousID string, old PluginDelivery) {
	t.Helper()
	if err := f.AddExternalPlugin(testExecutable(t), nil); err != nil {
		t.Fatal(err)
	}
	after, err := f.PluginStatus()
	if err != nil || after.ID == previousID || after.Enabled {
		t.Fatal("re-add reused identity or enabled state", after, err)
	}
	if err := f.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := f.GrantPluginAccess(PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	if err := testFeedAck(f, old); model.ErrorCodeOf(err) != model.CodeConflict {
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

func prepareNamedPluginRemoval(t *testing.T, f PluginFeed) PluginDelivery {
	t.Helper()
	if err := f.ConfigurePluginScope("all"); err != nil {
		t.Fatal(err)
	}
	if err := f.GrantPluginAccess(PluginAccess{Status: true}); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	old, err := nextFeed(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	return old
}

// setScope changes an enabled plugin's scope (disable, configure, enable)
// and acks the new gap.
