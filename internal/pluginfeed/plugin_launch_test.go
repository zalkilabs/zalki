package pluginfeed

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func externalPinFixture(t *testing.T) (PluginFeed, string, string) {
	t.Helper()
	s := fixtureStore(t)
	v, _ := register(t, s, "codex")
	path := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f := NewPluginFeed(s, "external")
	if err := f.AddExternalPlugin(path, []string{"$HOME", "literal space"}); err != nil {
		t.Fatal(err)
	}
	if err := f.ConfigurePluginFeed([]string{v.ID}); err != nil {
		t.Fatal(err)
	}
	return f, path, v.ID
}

func TestExternalPluginFileAndArgvDriftCheck(t *testing.T) {
	f, path, _ := externalPinFixture(t)
	status, err := f.PluginStatus()
	if err != nil || status.Launch == nil {
		t.Fatal(status, err)
	}
	if err := status.Launch.Verify(); err != nil {
		t.Fatal("fresh pin refused", err)
	}
	changed := *status.Launch
	changed.Args = slices.Clone(changed.Args)
	changed.Args[0] = "changed"
	if err := changed.Verify(); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("arguments were not pinned", err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := status.Launch.Verify(); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("executable digest drift accepted", err)
	}
	after, err := f.PluginStatus()
	if err != nil || after.Launch.SHA256 != status.Launch.SHA256 || after.Launch.PinSHA256 != status.Launch.PinSHA256 {
		t.Fatal("read silently repinned", after, err)
	}
}

func TestExternalPluginArgvBoundsAndLiteralShellText(t *testing.T) {
	path := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		args  []string
		valid bool
	}{
		{"eight", make([]string, 8), true}, {"nine", make([]string, 9), false},
		{"1024-bytes", []string{strings.Repeat("x", 1024)}, true}, {"1025-bytes", []string{strings.Repeat("x", 1025)}, false},
		{"utf8-1024-bytes", []string{strings.Repeat("é", 512)}, true}, {"utf8-1026-bytes", []string{strings.Repeat("é", 513)}, false},
		{"nul", []string{"a\x00b"}, false}, {"invalid-utf8", []string{string([]byte{0xff})}, false},
		{"literal-shell", []string{"$(touch never)", "$HOME", ";", "", "a b"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := PluginLaunch{Executable: path, Args: tc.args}
			err := l.pin()
			if (err == nil) != tc.valid {
				t.Fatal("argv bound", err)
			}
			if tc.valid && !slices.Equal(l.Args, tc.args) {
				t.Fatal("pin changed literal arguments")
			}
		})
	}
	for _, bad := range []string{"relative", "", path + "\x00"} {
		l := PluginLaunch{Executable: bad}
		if err := l.pin(); err == nil {
			t.Fatal("invalid executable accepted", bad)
		}
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	l := PluginLaunch{Executable: link}
	if err := l.pin(); err == nil {
		t.Fatal("followed executable symlink")
	}
}

func TestExternalPluginEnableRetryDoesNotSilentlyRepin(t *testing.T) {
	f, path, _ := externalPinFixture(t)
	denyPluginDirectoryRead(t, f.store)
	if err := f.EnablePluginFeed(true); !errors.Is(err, os.ErrPermission) {
		t.Fatal("expected unknown rename outcome", err)
	}
	p, _, err := f.loadPlugin()
	if err != nil || !p.Enabled || p.GapID == "" {
		t.Fatal("cut did not publish enabled pin", p, err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); !errors.Is(err, os.ErrPermission) {
		t.Fatal("retry skipped directory confirmation", err)
	}
	if err := os.Chmod(f.pluginDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	t.Run("confirm-and-repin", (&externalRetryCheck{f, p}).run)
}

func TestExternalPluginPinCannotCrossReconfigureOrReadd(t *testing.T) {
	t.Run("grants", pinAcrossGrantsSubtest)
	t.Run("readd", pinAcrossReaddSubtest)
}

func pinAcrossGrantsSubtest(t *testing.T) {
	f, _, id := externalPinFixture(t)
	pin, prior, err := f.preparePluginPin()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.ConfigurePluginFeed([]string{id, "codex:" + model.NewUUID()}); err != nil {
		t.Fatal(err)
	}
	if err := f.enablePluginFeed(true, pin, prior); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("stale pin crossed record change", err)
	}
	status, err := f.PluginStatus()
	if err != nil || status.Enabled {
		t.Fatal("stale pin enabled installation", status, err)
	}
}

func pinAcrossReaddSubtest(t *testing.T) {
	f, path, id := externalPinFixture(t)
	pin, prior, err := f.preparePluginPin()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	if err := f.AddExternalPlugin(path, pin.Args); err != nil {
		t.Fatal(err)
	}
	if err := f.ConfigurePluginFeed([]string{id, "codex:" + model.NewUUID()}); err != nil {
		t.Fatal(err)
	}
	if err := f.enablePluginFeed(true, pin, prior); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("stale pin crossed record change", err)
	}
	status, err := f.PluginStatus()
	if err != nil || status.Enabled {
		t.Fatal("stale pin enabled installation", status, err)
	}
}

func TestExternalPluginRejectsUncleanExecutablePath(t *testing.T) {
	for _, path := range []string{"/a/../b", "//", "//a/b", "/a//b", "/a/./b", "/a/b/"} {
		t.Run(path, func(t *testing.T) {
			l := PluginLaunch{Executable: path}
			if err := l.validate(); model.ErrorCodeOf(err) != model.CodeInvalidInput {
				t.Fatal("unclean executable path accepted", path, err)
			}
		})
	}
}

func zeroArgExternalFeed(t *testing.T) PluginFeed {
	t.Helper()
	s := fixtureStore(t)
	path := filepath.Join(t.TempDir(), "plugin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	f := NewPluginFeed(s, "external")
	if err := f.AddExternalPlugin(path, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestExternalPluginZeroArgDriftPinSurvivesJSONRoundTrip(t *testing.T) {
	f := zeroArgExternalFeed(t)
	status, err := f.PluginStatus()
	if err != nil || status.Launch == nil {
		t.Fatal(status, err)
	}
	raw, err := json.Marshal(status.Launch)
	if err != nil {
		t.Fatal(err)
	}
	var decoded PluginLaunch
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Args == nil || len(decoded.Args) != 0 {
		t.Fatal("zero argv was not normalized to []", decoded.Args)
	}
	if err := decoded.Verify(); err != nil {
		t.Fatal("zero-argument drift pin broke after JSON", err)
	}
}

func rewritePluginLaunchRecord(t *testing.T, f PluginFeed, edit func(*PluginLaunch)) {
	t.Helper()
	p, _, err := f.loadPlugin()
	if err != nil || p.Launch == nil {
		t.Fatal(p, err)
	}
	edit(p.Launch)
	raw, err := journal.EncodeJSON(p, 16<<10)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.WriteAtomic(f.pluginPath(), raw); err != nil {
		t.Fatal(err)
	}
}

func TestExternalPluginEditedNullArgvFailsDriftVerification(t *testing.T) {
	f := zeroArgExternalFeed(t)
	rewritePluginLaunchRecord(t, f, func(l *PluginLaunch) { l.Args = nil })
	status, err := f.PluginStatus()
	if err != nil {
		if !errors.Is(err, journal.ErrInvalidBatch) && model.ErrorCodeOf(err) != model.CodeInvalidInput {
			t.Fatal("unexpected load failure", err)
		}
		return
	}
	if status.Launch == nil {
		t.Fatal("edited launch disappeared")
	}
	if err := status.Launch.Verify(); err == nil {
		t.Fatal("hand-edited args:null reused the [] drift pin")
	}
}

func TestExternalPluginPinIsDriftCheckNotAuthenticity(t *testing.T) {
	f := zeroArgExternalFeed(t)
	rewritePluginLaunchRecord(t, f, func(l *PluginLaunch) {
		l.Args = []string{"rewritten-by-record-owner"}
		if err := l.pin(); err != nil {
			t.Fatal(err)
		}
	})
	status, err := f.PluginStatus()
	if err != nil || status.Launch == nil {
		t.Fatal(status, err)
	}
	if err := status.Launch.Verify(); err != nil {
		t.Fatal("drift check claimed authenticity against record owner", err)
	}
}

type externalRetryCheck struct {
	feed     PluginFeed
	previous pluginRecord
}

func (check *externalRetryCheck) run(t *testing.T) {
	f, p := check.feed, check.previous
	after, _, err := f.loadPlugin()
	if err != nil || after.GapID != p.GapID || after.Launch.SHA256 != p.Launch.SHA256 || after.Launch.PinSHA256 != p.Launch.PinSHA256 {
		t.Fatal("retry silently repinned or replaced gap", after, err)
	}
	if err := after.Launch.Verify(); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("changed file matched retry pin", err)
	}
	if err := f.EnablePluginFeed(false); err != nil {
		t.Fatal(err)
	}
	if err := f.EnablePluginFeed(true); err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	if err := status.Launch.Verify(); err != nil {
		t.Fatal("explicit disable/enable did not repin", err)
	}
}
