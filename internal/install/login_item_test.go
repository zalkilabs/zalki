package install

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeLaunchd answers launchctl like the service manager would, from a
// small model, and records every call. Nothing reaches the real OS.
type fakeLaunchd struct {
	loaded, disabled, domainFails, printFails, malformed, bootoutFails bool
	disabledFails, disabledMalformed, domainMalformed                  bool
	truncated, duplicated                                              bool
	unknownVerb                                                        string
	bootstrapFails                                                     bool
	onPrint                                                            func()
	onMutate                                                           func(verb string)
	disabledFailsAfter, disabledCalls                                  int
	domainVariant                                                      string
	program                                                            string
	args                                                               []string
	calls                                                              []string
	onBootout                                                          func()
}

func (f *fakeLaunchd) run(_ context.Context, args ...string) ([]byte, int, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	label := domain() + "/" + loginItemLabel
	switch {
	case args[0] == f.unknownVerb:
		return nil, 0, errors.New("no exit status") // the change may or may not have happened
	case args[0] == "print-disabled":
		return f.printDisabled()
	case args[0] == "print" && args[1] == label:
		if f.onPrint != nil {
			f.onPrint()
		}
		return f.printJob(label)
	case args[0] == "print":
		return f.printDomain()
	}
	return f.mutate(args[0])
}

func (f *fakeLaunchd) printDisabled() ([]byte, int, error) {
	f.disabledCalls++
	switch {
	case f.disabledFails, f.disabledFailsAfter > 0 && f.disabledCalls > f.disabledFailsAfter:
		return nil, 5, nil
	case f.disabledMalformed:
		return []byte("disabled services = {\n\tsomething new\n}\n"), 0, nil
	}
	value := "enabled"
	if f.disabled {
		value = "disabled"
	}
	return fmt.Appendf(nil, "disabled services = {\n\t\"%s\" => %s\n}\n", loginItemLabel, value), 0, nil
}

func (f *fakeLaunchd) printDomain() ([]byte, int, error) {
	switch {
	case f.domainFails:
		return nil, 5, nil
	case f.domainVariant == "opaque":
		return []byte("something new\n"), 0, nil
	case f.domainVariant == "headerless":
		return []byte("\tservices = {\n\t\t0\t-\tcom.example.other\n\t}\n}\n"), 0, nil
	case f.domainVariant == "other uid":
		return []byte("gui/1 = {\n\tservices = {\n\t\t0\t-\tcom.example.other\n\t}\n}\n"), 0, nil
	case f.domainVariant == "truncated":
		return []byte(domain() + " = {\n\tservices = {\n\t\t0\t-\tcom.example.other\n\t}\n"), 0, nil
	}
	services := "\t\t0\t-\tcom.example.other\n"
	if f.loaded {
		services += "\t\t0\t-\t" + loginItemLabel + "\n"
	}
	if f.domainMalformed {
		services += "\t\t?\t" + loginItemLabel + "\n"
	}
	return []byte(domain() + " = {\n\tservices = {\n" + services + "\t}\n}\n"), 0, nil
}

func (f *fakeLaunchd) mutate(verb string) ([]byte, int, error) {
	if f.onMutate != nil {
		f.onMutate(verb)
	}
	switch {
	case verb == "bootstrap" && f.bootstrapFails:
		return nil, 5, nil
	case verb == "bootstrap":
		f.loaded = true
	case verb == "bootout":
		if f.onBootout != nil {
			f.onBootout()
		}
		if f.bootoutFails {
			return nil, 5, nil
		}
		f.loaded = false
	}
	return nil, 0, nil
}

// printJob is the fake "launchctl print" output for the item's own label.
func (f *fakeLaunchd) printJob(label string) ([]byte, int, error) {
	if f.printFails || !f.loaded {
		return nil, 113, nil
	}
	if f.malformed {
		return []byte("something new\n"), 0, nil
	}
	var out strings.Builder
	out.WriteString(label + " = {\n\tstate = not running\n\tprogram = " + f.program + "\n\targuments = {\n")
	if f.duplicated {
		out.WriteString("\t}\n\tprogram = /tmp/other\n\targuments = {\n")
	}
	for _, arg := range f.args {
		out.WriteString("\t\t" + arg + "\n")
	}
	out.WriteString("\t}\n")
	if !f.truncated {
		out.WriteString("}\n")
	}
	return []byte(out.String()), 0, nil
}

func (f *fakeLaunchd) mutations() []string {
	var out []string
	for _, call := range f.calls {
		if !strings.HasPrefix(call, "print") {
			out = append(out, strings.Fields(call)[0])
		}
	}
	return out
}

type loginFixture struct {
	home, state, binary string
	fake                *fakeLaunchd
	paths               loginItemPaths
}

func newLoginFixture(t *testing.T) *loginFixture {
	t.Helper()
	home := t.TempDir()
	f := &loginFixture{home: home, state: filepath.Join(home, ".local", "state", "zalki"), binary: "/opt/homebrew/bin/zalki", fake: &fakeLaunchd{}}
	f.fake.program, f.fake.args = f.binary, []string{f.binary, "plugins", "supervise", "--state", f.state}
	f.paths = newLoginItemPaths(home, f.state)
	oldHome, oldRun, oldSupported := accountHome, runLaunchctl, loginItemsSupported
	accountHome, runLaunchctl, loginItemsSupported = func() (string, error) { return home, nil }, f.fake.run, true
	t.Cleanup(func() { accountHome, runLaunchctl, loginItemsSupported = oldHome, oldRun, oldSupported })
	return f
}

func (f *loginFixture) do(action string, apply bool) (Report, error) {
	request := Request{Action: action, Binary: f.binary, StateDir: f.state, HomeDir: f.home, Apply: apply}
	if action == "relaunch" || action == "install" { // the owner was shown the item as it is now
		request.ExpectRecord = recordToken(readLoginItemFiles(f.paths))
	}
	return ManagedLoginItem(context.Background(), request)
}

// doExpect runs a change bound to the record the owner was shown.
func (f *loginFixture) doExpect(action, expect string) (Report, error) {
	return ManagedLoginItem(context.Background(), Request{Action: action, Binary: f.binary, StateDir: f.state, HomeDir: f.home, ExpectRecord: expect, Apply: true})
}

const plistGolden = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>dev.zalki.plugins</string>
  <key>ProgramArguments</key>
  <array>
    <string>/opt/homebrew/bin/zalki</string>
    <string>plugins</string>
    <string>supervise</string>
    <string>--state</string>
    <string>/home/example/.local/state/zalki</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
</dict>
</plist>
`

// S04: the plist is the contract's exact golden, values are XML-escaped,
// and no environment, log path or other key is ever written.
func TestPluginLoginItemPlistGolden(t *testing.T) {
	if got := string(renderLoginItem("/opt/homebrew/bin/zalki", "/home/example/.local/state/zalki")); got != plistGolden {
		t.Fatalf("plist differs from the golden:\n%s", got)
	}
	escaped := string(renderLoginItem("/opt/a&b/<zalki>", "/home/x/.local/state/zalki"))
	if !strings.Contains(escaped, "<string>/opt/a&amp;b/&lt;zalki&gt;</string>") {
		t.Fatal("a launcher path was not XML-escaped")
	}
	for _, key := range []string{"EnvironmentVariables", "StandardOutPath", "StandardErrorPath", "ThrottleInterval", "Caskroom"} {
		if strings.Contains(plistGolden, key) {
			t.Fatalf("forbidden key %s", key)
		}
	}
}

// S02: a scratch HOME or any state but the default one never reaches the
// real item: no file, no lock, no launchctl call.
func TestPluginLoginItemDefaultGuard(t *testing.T) {
	f := newLoginFixture(t)
	for name, request := range map[string]Request{
		"custom state":  {Action: "install", Binary: f.binary, StateDir: filepath.Join(f.home, "custom"), HomeDir: f.home, Apply: true},
		"scratch home":  {Action: "install", Binary: f.binary, StateDir: filepath.Join(t.TempDir(), ".local", "state", "zalki"), HomeDir: t.TempDir(), Apply: true},
		"relative path": {Action: "install", Binary: "zalki", StateDir: f.state, HomeDir: f.home, Apply: true},
	} {
		if _, err := ManagedLoginItem(context.Background(), request); !errors.Is(err, errLoginItemNotOwned) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(f.fake.calls) != 0 {
		t.Fatalf("a refused request called launchctl: %v", f.fake.calls)
	}
	if _, err := os.Lstat(f.paths.plist); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused request wrote the plist")
	}
}

// S20: inspect reports only what it can prove and never changes anything.
func TestPluginBackgroundStatusEvidence(t *testing.T) {
	for name, test := range map[string]struct {
		set            func(*fakeLaunchd)
		state, matched string
	}{
		"loaded idle":    {func(l *fakeLaunchd) { l.loaded = true }, serviceLoaded, loadedMatches},
		"absent":         {func(*fakeLaunchd) {}, serviceNotLoaded, loadedUnknown},
		"print failed":   {func(l *fakeLaunchd) { l.loaded, l.printFails, l.domainFails = true, true, true }, serviceUnknown, loadedUnknown},
		"domain unknown": {func(l *fakeLaunchd) { l.domainFails = true }, serviceUnknown, loadedUnknown},
		"malformed":      {func(l *fakeLaunchd) { l.loaded, l.malformed = true, true }, serviceUnknown, loadedUnknown},
		"disabled":       {func(l *fakeLaunchd) { l.disabled = true }, serviceDisabled, loadedUnknown},
		"other program":  {func(l *fakeLaunchd) { l.loaded, l.program = true, "/tmp/other" }, serviceLoaded, loadedMismatch},
		"extra argument": {func(l *fakeLaunchd) { l.loaded = true; l.args = append(slices.Clone(l.args), "--x") }, serviceLoaded, loadedMismatch},
		"different state": {func(l *fakeLaunchd) {
			l.loaded = true
			l.args = []string{l.program, "plugins", "supervise", "--state", "/tmp/s"}
		}, serviceLoaded, loadedMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			f := newLoginFixture(t)
			test.set(f.fake)
			report, err := f.do("inspect", false)
			if err != nil || report.ServiceState != test.state || report.LoadedIdentity != test.matched || report.ManagedState != itemMissing {
				t.Fatalf("inspect = %s/%s/%s, want %s/%s (%v)", report.ServiceState, report.LoadedIdentity, report.ManagedState, test.state, test.matched, err)
			}
			if len(f.fake.mutations()) != 0 {
				t.Fatalf("inspect changed launchd: %v", f.fake.mutations())
			}
			if _, err := os.Lstat(filepath.Join(f.state, "plugins")); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("inspect created state directories")
			}
		})
	}
}

// S05: first setup writes the exact plist and its record, then bootstraps;
// a foreign file (even byte-identical), a changed one or a damaged record is
// named and kept byte for byte, with no launchctl change.
func TestPluginLoginItemOwnershipAndRecovery(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.paths.plist)
	if err != nil || string(raw) != string(renderLoginItem(f.binary, f.state)) || !slices.Equal(f.fake.mutations(), []string{"bootstrap"}) {
		t.Fatalf("first setup: %v, mutations %v", err, f.fake.mutations())
	}
	if report, _ := f.do("inspect", false); report.ManagedState != itemManaged || report.OperationPending {
		t.Fatalf("after setup: %+v", report)
	}
	for name, plant := range map[string]func(*loginFixture){
		"foreign identical": func(f *loginFixture) { writeFile(t, f.paths.plist, renderLoginItem(f.binary, f.state)) },
		"changed": func(f *loginFixture) {
			if _, err := f.do("install", true); err != nil {
				t.Fatal(err)
			}
			writeFile(t, f.paths.plist, []byte("changed"))
		},
		"damaged record": func(f *loginFixture) {
			if _, err := f.do("install", true); err != nil {
				t.Fatal(err)
			}
			writeFile(t, f.paths.record, []byte("{"))
		},
		"unfinished operation": func(f *loginFixture) {
			plist := renderLoginItem(f.binary, f.state)
			desired := &loginItemRecord{Schema: 1, UID: os.Getuid(), Home: f.home, State: f.state, Label: loginItemLabel, Path: f.paths.plist, Binary: f.binary, PlistSHA: sha256Hex(plist), Generation: 1}
			mkdir(t, filepath.Dir(f.paths.pending))
			if err := writePrivateJSON(f.paths.pending, loginItemPending{Schema: 1, Operation: "x", Action: "install", Phase: "prepared", Desired: desired}); err != nil {
				t.Fatal(err)
			}
			writeFile(t, f.paths.plist, plist)
		},
	} {
		t.Run(name, func(t *testing.T) { checkLoginItemKept(t, plant) })
	}
}

func checkLoginItemKept(t *testing.T, plant func(*loginFixture)) {
	t.Helper()
	f := newLoginFixture(t)
	mkdir(t, filepath.Dir(f.paths.plist))
	plant(f)
	f.fake.calls = nil
	before := snapshotFiles(t, f.paths)
	for _, action := range []string{"install", "uninstall"} {
		if _, err := f.do(action, true); err == nil {
			t.Fatalf("%s over a file zalki cannot prove it owns succeeded", action)
		}
	}
	if after := snapshotFiles(t, f.paths); after != before || len(f.fake.mutations()) != 0 {
		t.Fatalf("a refused change touched files or launchd: %v", f.fake.mutations())
	}
}

// S20/S23: first setup needs positive absence; a disabled, unknown or
// different loaded job is never bootstrapped over.
func TestPluginLoginItemNeverOverridesTheOS(t *testing.T) {
	for name, set := range map[string]func(*fakeLaunchd){
		"disabled":      func(l *fakeLaunchd) { l.disabled = true },
		"unknown":       func(l *fakeLaunchd) { l.domainFails = true },
		"other program": func(l *fakeLaunchd) { l.loaded, l.program = true, "/tmp/other" },
	} {
		f := newLoginFixture(t)
		set(f.fake)
		if _, err := f.do("install", true); err == nil || len(f.fake.mutations()) != 0 {
			t.Errorf("%s: install changed launchd: %v %v", name, err, f.fake.mutations())
		}
		if _, err := os.Lstat(f.paths.plist); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s: install wrote the plist", name)
		}
	}
}

// S23: uninstall stops the exact job BEFORE deleting the owned plist; a
// failed stop keeps the plist, the record and the pending ownership.
func TestPluginLoginItemUninstallOrder(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	f.fake.bootoutFails = true
	if _, err := f.do("uninstall", true); err == nil {
		t.Fatal("an unconfirmed stop was reported as removed")
	}
	if report, _ := f.do("inspect", false); report.ManagedState != itemManaged || !report.OperationPending {
		t.Fatalf("a failed stop lost ownership: %+v", report)
	}
	f.fake.bootoutFails = false
	presentAtBootout := false
	f.fake.onBootout = func() { _, err := os.Lstat(f.paths.plist); presentAtBootout = err == nil }
	if _, err := f.do("uninstall", true); err != nil {
		t.Fatal(err)
	}
	if !presentAtBootout {
		t.Fatal("the plist was deleted before the job was stopped")
	}
	for _, path := range []string{f.paths.plist, f.paths.record, f.paths.pending} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s stayed after a confirmed uninstall", filepath.Base(path))
		}
	}
}

// Q5: a held operation lock makes another change wait at most its bound
// and give up as busy, changing nothing.
func TestPluginLoginItemOperationLock(t *testing.T) {
	f := newLoginFixture(t)
	mkdir(t, filepath.Dir(f.paths.lock))
	fd, err := syscall.Open(f.paths.lock, syscall.O_CREAT|syscall.O_RDWR, 0o600)
	if err == nil {
		err = syscall.Flock(fd, syscall.LOCK_EX)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(fd)
	if _, err := f.do("install", true); !errors.Is(err, errLoginItemBusy) || len(f.fake.calls) != 0 {
		t.Fatalf("a held operation lock did not refuse as busy: %v", err)
	}
}

func writeFile(t *testing.T, path string, raw []byte) {
	t.Helper()
	mkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func snapshotFiles(t *testing.T, p loginItemPaths) string {
	t.Helper()
	var b strings.Builder
	for _, path := range []string{p.plist, p.record, p.pending} {
		raw, err := os.ReadFile(path)
		fmt.Fprintf(&b, "%s:%q:%v;", filepath.Base(path), raw, err == nil)
	}
	return b.String()
}

// A record with the right meaning but not the exact bytes Zalki writes (a
// duplicate key, here) is damaged, never ownership.
func TestPluginLoginItemRecordMustBeCanonical(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.paths.record)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), `"schema":1,`, `"schema":1,"schema":1,`, 1)
	writeFile(t, f.paths.record, []byte(edited))
	if report, _ := f.do("inspect", false); report.ManagedState != itemInvalidRecord {
		t.Fatalf("a hand-edited record was accepted: %s", report.ManagedState)
	}
}

// Nothing is written through a LaunchAgents folder that is a link or that
// others can write.
func TestPluginLoginItemRefusesUnsafeLaunchAgents(t *testing.T) {
	for name, plant := range map[string]func(f *loginFixture, target string) error{
		"link": func(f *loginFixture, target string) error {
			mkdir(t, filepath.Dir(filepath.Dir(f.paths.plist)))
			return os.Symlink(target, filepath.Dir(f.paths.plist))
		},
		"writable": func(f *loginFixture, _ string) error {
			mkdir(t, filepath.Dir(f.paths.plist))
			return os.Chmod(filepath.Dir(f.paths.plist), 0o777)
		},
	} {
		f := newLoginFixture(t)
		target := t.TempDir()
		if err := plant(f, target); err != nil {
			t.Fatal(err)
		}
		_, err := f.do("install", true)
		entries, _ := os.ReadDir(target)
		_, plistErr := os.Lstat(f.paths.plist)
		_, pendingErr := os.Lstat(f.paths.pending)
		if err == nil || len(entries) != 0 || !errors.Is(plistErr, os.ErrNotExist) || !errors.Is(pendingErr, os.ErrNotExist) || slices.Contains(f.fake.mutations(), "bootstrap") {
			t.Fatalf("%s: the plist was written through an unsafe LaunchAgents folder: %v", name, err)
		}
	}
}

// S23/Q2: uninstall never boots out a job it does not own: a different
// program under the same label stays loaded and the owned files are kept.
func TestPluginLoginItemUninstallLeavesForeignLoadedJob(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	f.fake.program, f.fake.calls = "/tmp/other", nil
	before := snapshotFiles(t, f.paths)
	if _, err := f.do("uninstall", true); err == nil || slices.Contains(f.fake.mutations(), "bootout") || snapshotFiles(t, f.paths) != before {
		t.Fatalf("a different loaded job was booted out or the owned files changed: %v %v", err, f.fake.mutations())
	}

}

// An owned item that runs another launcher path is named and kept: it is
// never restarted from the old plist or reported as a foreign job.
func TestPluginLoginItemRefusesAnotherLauncher(t *testing.T) {
	for _, loaded := range []bool{true, false} {
		f := newLoginFixture(t)
		if _, err := f.do("install", true); err != nil {
			t.Fatal(err)
		}
		f.fake.loaded, f.fake.calls = loaded, nil
		before := snapshotFiles(t, f.paths)
		old := f.binary
		f.binary = "/usr/local/bin/zalki"
		want := "The zalki login item runs " + old + ", not " + f.binary + "; nothing was changed."
		_, err := f.do("install", true)
		if err == nil || err.Error() != want || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
			t.Fatalf("loaded=%v: another launcher was not refused before any change: %v %v", loaded, err, f.fake.mutations())
		}
	}
}

// S23 with an OS-disabled label: a different or unreadable loaded job is
// never booted out, and nothing is written, whatever the disabled state.
func TestPluginLoginItemDisabledLabelKeepsForeignJob(t *testing.T) {
	for name, tc := range map[string]struct {
		plant func(*fakeLaunchd)
		want  error
	}{
		"disabled foreign":    {func(l *fakeLaunchd) { l.disabled, l.program = true, "/tmp/other" }, errLoginItemLoaded},
		"disabled unreadable": {func(l *fakeLaunchd) { l.disabled, l.malformed = true, true }, errLoginItemUnknown},
	} {
		f := newLoginFixture(t)
		if _, err := f.do("install", true); err != nil {
			t.Fatal(err)
		}
		tc.plant(f.fake)
		f.fake.calls = nil
		before := snapshotFiles(t, f.paths)
		if _, err := f.do("uninstall", true); !errors.Is(err, tc.want) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
			t.Fatalf("%s: a disabled label let a job that is not ours be stopped or the files change: %v %v", name, err, f.fake.mutations())
		}
	}
}

// Our own job under a disabled label is still removed: booted out when it
// is loaded, only deleted when the domain shows it is not.
func TestPluginLoginItemDisabledOwnedJobIsRemoved(t *testing.T) {
	for _, loaded := range []bool{true, false} {
		f := newLoginFixture(t)
		if _, err := f.do("install", true); err != nil {
			t.Fatal(err)
		}
		f.fake.disabled, f.fake.loaded, f.fake.calls = true, loaded, nil
		_, err := f.do("uninstall", true)
		_, plistErr := os.Lstat(f.paths.plist)
		if err != nil || slices.Contains(f.fake.mutations(), "bootout") != loaded || !errors.Is(plistErr, os.ErrNotExist) {
			t.Fatalf("loaded=%v: our disabled job was not removed: %v %v", loaded, err, f.fake.mutations())
		}
	}
}

// S20: a first setup never adopts a loaded job it has no record of, even
// one that runs this launcher.
func TestPluginLoginItemFirstSetupRefusesUnrecordedJob(t *testing.T) {
	f := newLoginFixture(t)
	f.fake.loaded = true
	before := snapshotFiles(t, f.paths)
	if _, err := f.do("install", true); !errors.Is(err, errLoginItemUnowned) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
		t.Fatalf("a first setup adopted an unrecorded loaded job: %v %v", err, f.fake.mutations())
	}
}

// M4: an unreadable discovery (disabled query failed or unrecognised, or a
// service row it cannot read) is unknown: no first setup, no writes.
func TestPluginLoginItemUnreadableDiscoveryIsUnknown(t *testing.T) {
	for name, set := range map[string]func(*fakeLaunchd){
		"disabled query failed": func(l *fakeLaunchd) { l.disabledFails = true },
		"disabled list unknown": func(l *fakeLaunchd) { l.disabledMalformed = true },
		"domain row unreadable": func(l *fakeLaunchd) { l.domainMalformed = true },
		"domain opaque":         func(l *fakeLaunchd) { l.domainVariant = "opaque" },
		"domain headerless":     func(l *fakeLaunchd) { l.domainVariant = "headerless" },
		"domain truncated":      func(l *fakeLaunchd) { l.domainVariant = "truncated" },
		"domain of another uid": func(l *fakeLaunchd) { l.domainVariant = "other uid" },
	} {
		f := newLoginFixture(t)
		set(f.fake)
		before := snapshotFiles(t, f.paths)
		if _, err := f.do("install", true); !errors.Is(err, errLoginItemUnknown) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
			t.Errorf("%s: an unreadable discovery allowed a first setup: %v %v", name, err, f.fake.mutations())
		}
	}
}

// M3: a resumed uninstall still proves the files are ours: a changed
// plist or a damaged or different committed record stops it before any
// launchctl change.
func TestPluginLoginItemResumedUninstallChecksFiles(t *testing.T) {
	for name, damage := range map[string]func(f *loginFixture){
		"changed plist":  func(f *loginFixture) { writeFile(t, f.paths.plist, []byte("changed")) },
		"damaged record": func(f *loginFixture) { writeFile(t, f.paths.record, []byte("{")) },
		"other record": func(f *loginFixture) {
			var record loginItemRecord
			if _, err := readPrivateJSON(f.paths.record, &record); err != nil {
				t.Fatal(err)
			}
			record.Generation++
			if err := writePrivateJSON(f.paths.record, record); err != nil {
				t.Fatal(err)
			}
		},
	} {
		f := newLoginFixture(t)
		if _, err := f.do("install", true); err != nil {
			t.Fatal(err)
		}
		f.fake.bootoutFails = true
		if _, err := f.do("uninstall", true); err == nil {
			t.Fatal("the first uninstall should fail at the stop")
		}
		damage(f)
		f.fake.bootoutFails, f.fake.calls = false, nil
		before := snapshotFiles(t, f.paths)
		if _, err := f.do("uninstall", true); err == nil || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
			t.Errorf("%s: a resumed uninstall acted on files that are not ours: %v %v", name, err, f.fake.mutations())
		}
	}
}

// M3: the pending record is checked as a whole: token, action, phase and
// the identity each action may recover to.
func TestPluginLoginItemPendingMustBeValid(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	var record loginItemRecord
	if _, err := readPrivateJSON(f.paths.record, &record); err != nil {
		t.Fatal(err)
	}
	other := record
	other.Home = "/home/other"
	// Without a committed record, an uninstall would act on the pending
	// prior alone: so the prior itself must be checked.
	if err := os.Remove(f.paths.record); err != nil {
		t.Fatal(err)
	}
	op := strings.Repeat("a", 32)
	for name, pending := range map[string]loginItemPending{
		"short token":         {Schema: 1, Operation: "abc", Action: "uninstall", Phase: "prepared", Prior: &record},
		"unknown action":      {Schema: 1, Operation: op, Action: "repair", Phase: "prepared", Prior: &record},
		"unknown phase":       {Schema: 1, Operation: op, Action: "uninstall", Phase: "done", Prior: &record},
		"uninstall no prior":  {Schema: 1, Operation: op, Action: "uninstall", Phase: "prepared"},
		"install with prior":  {Schema: 1, Operation: op, Action: "install", Phase: "prepared", Prior: &record, Desired: &record},
		"prior of other home": {Schema: 1, Operation: op, Action: "uninstall", Phase: "prepared", Prior: &other},
	} {
		if err := writePrivateJSON(f.paths.pending, pending); err != nil {
			t.Fatal(err)
		}
		f.fake.calls = nil
		before := snapshotFiles(t, f.paths)
		if _, err := f.do("uninstall", true); !errors.Is(err, errInvalidLoginRecord) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
			t.Errorf("%s: an invalid pending record was trusted: %v %v", name, err, f.fake.mutations())
		}
	}
}

// M5: every folder from the account home down is checked: a linked
// ancestor is refused and nothing is created through it.
func TestPluginLoginItemRefusesLinkedAncestors(t *testing.T) {
	for _, ancestor := range []string{"Library", ".local"} {
		f := newLoginFixture(t)
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(f.home, ancestor)); err != nil {
			t.Fatal(err)
		}
		_, err := f.do("install", true)
		entries, _ := os.ReadDir(target)
		if err == nil || len(entries) != 0 || len(f.fake.mutations()) != 0 {
			t.Errorf("%s: a linked ancestor was followed: %v %d entries %v", ancestor, err, len(entries), f.fake.mutations())
		}
	}
}

// M5: after an install, a LaunchAgents folder swapped for a link to a copy
// is never read as ours or emptied at uninstall.
func TestPluginLoginItemUninstallRefusesLinkedParent(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "agents")
	if err := os.Rename(filepath.Dir(f.paths.plist), target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Dir(f.paths.plist)); err != nil {
		t.Fatal(err)
	}
	f.fake.calls = nil
	_, err := f.do("uninstall", true)
	if _, statErr := os.Stat(filepath.Join(target, filepath.Base(f.paths.plist))); err == nil || statErr != nil || len(f.fake.mutations()) != 0 {
		t.Fatalf("an uninstall acted through a linked LaunchAgents folder: %v %v %v", err, statErr, f.fake.mutations())
	}
}

// M5: a planted FIFO or a shared file at the lock path is refused before
// any operation.
func TestPluginLoginItemRefusesUnsafeLock(t *testing.T) {
	for name, plant := range map[string]func(path string) error{
		"fifo":   func(path string) error { return syscall.Mkfifo(path, 0o600) },
		"shared": func(path string) error { return os.WriteFile(path, nil, 0o644) },
	} {
		f := newLoginFixture(t)
		if err := os.MkdirAll(filepath.Dir(f.paths.lock), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := plant(f.paths.lock); err != nil {
			t.Fatal(err)
		}
		if _, err := f.do("install", true); !errors.Is(err, errLoginItemLockUnsafe) || len(f.fake.mutations()) != 0 {
			t.Errorf("%s: an unsafe lock file was used: %v %v", name, err, f.fake.mutations())
		}
	}
}

// M4: job output that is cut off or carries a second program is unknown:
// uninstall stops before any write or launchctl change.
func TestPluginLoginItemJobOutputMustBeComplete(t *testing.T) {
	for name, set := range map[string]func(*fakeLaunchd){
		"truncated":  func(l *fakeLaunchd) { l.truncated = true },
		"duplicated": func(l *fakeLaunchd) { l.duplicated = true },
	} {
		f := newLoginFixture(t)
		if _, err := f.do("install", true); err != nil {
			t.Fatal(err)
		}
		set(f.fake)
		f.fake.calls = nil
		before := snapshotFiles(t, f.paths)
		if _, err := f.do("uninstall", true); !errors.Is(err, errLoginItemUnknown) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
			t.Errorf("%s: incomplete job output was trusted: %v %v", name, err, f.fake.mutations())
		}
	}
}

// Q5: a known bootstrap failure on first setup commits the written file (it
// is Zalki's) and says launchd did not start it.
func TestPluginLoginItemKnownFailureCommitsFile(t *testing.T) {
	f := newLoginFixture(t)
	f.fake.bootstrapFails = true
	_, err := f.do("install", true)
	report, _ := f.do("inspect", false)
	if err == nil || !strings.Contains(err.Error(), "launchd did not start it") || report.ManagedState != itemManaged || report.OperationPending {
		t.Fatalf("a known bootstrap failure was not committed as written: %v %+v", err, report)
	}
}

// Q5: a setup over an existing record (its plist gone) raises the
// generation; it never restarts at 1.
func TestPluginLoginItemGenerationIsMonotonic(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.paths.plist); err != nil {
		t.Fatal(err)
	}
	f.fake.loaded = false
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	var record loginItemRecord
	if _, err := readPrivateJSON(f.paths.record, &record); err != nil || record.Generation != 2 {
		t.Fatalf("a setup over a record did not raise the generation: %d %v", record.Generation, err)
	}
}

// Q5: the token is checked again right before the change: an operation
// taken over in between changes nothing more.
func TestPluginLoginItemRevalidatesTokenBeforeChange(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	prints := 0
	f.fake.onPrint = func() {
		if prints++; prints == 2 { // the fresh look right before bootout
			var pending loginItemPending
			if _, err := readPrivateJSON(f.paths.pending, &pending); err != nil {
				t.Error(err)
				return
			}
			pending.Operation = strings.Repeat("d", 32)
			if err := writePrivateJSON(f.paths.pending, pending); err != nil {
				t.Error(err)
			}
		}
	}
	f.fake.calls = nil
	if _, err := f.do("uninstall", true); !errors.Is(err, errLoginItemTakenOver) || slices.Contains(f.fake.mutations(), "bootout") {
		t.Fatalf("a taken-over operation still stopped the job: %v %v", err, f.fake.mutations())
	}
}

// S23 (in-process part of the gate): while an uninstall holds the
// operation lock between bootout and deletion, an install waits and gets
// busy; no bootstrap happens before the uninstall commits. The cross-process
// interleaving control is a 7d gate item.
func TestPluginLoginItemUninstallHoldsTheGate(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	var inner error
	f.fake.onBootout = func() { _, inner = f.do("install", true) }
	f.fake.calls = nil
	if _, err := f.do("uninstall", true); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(inner, errLoginItemBusy) || slices.Contains(f.fake.mutations(), "bootstrap") {
		t.Fatalf("an install ran inside the uninstall's gate: %v %v", inner, f.fake.mutations())
	}
}

// M5: inspect checks the record folder chain too: records behind a linked
// ancestor are a damaged record, never ownership.
func TestPluginLoginItemInspectRefusesLinkedRecordFolder(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(t.TempDir(), "local")
	if err := os.Rename(filepath.Join(f.home, ".local"), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, filepath.Join(f.home, ".local")); err != nil {
		t.Fatal(err)
	}
	if report, _ := f.do("inspect", false); report.ManagedState != itemInvalidRecord {
		t.Fatalf("records behind a linked folder were read as ownership: %+v", report)
	}
}

// M3: a record's hash must be the renderer's lowercase hash for its own
// launcher and state; anything else is a damaged record.
func TestPluginLoginItemRecordHashMustMatchRenderer(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	var record loginItemRecord
	if _, err := readPrivateJSON(f.paths.record, &record); err != nil {
		t.Fatal(err)
	}
	record.PlistSHA = strings.ToUpper(record.PlistSHA)
	if err := writePrivateJSON(f.paths.record, record); err != nil {
		t.Fatal(err)
	}
	f.fake.calls = nil
	if _, err := f.do("uninstall", true); !errors.Is(err, errInvalidLoginRecord) || len(f.fake.mutations()) != 0 {
		t.Fatalf("a record with a non-canonical hash was trusted: %v %v", err, f.fake.mutations())
	}
}

// Q5: a crash inside Zalki's own first setup never blocks later commands:
// install and uninstall finish the file transaction first (plist only
// with the recorded bytes, then the record).
func TestPluginLoginItemFinishesOwnInstall(t *testing.T) {
	for _, point := range []string{"pending only", "plist written", "record committed"} {
		for action, want := range map[string]string{"install": itemManaged, "uninstall": itemMissing} {
			f := newLoginFixture(t)
			plantOwnSetup(t, f, point)
			if _, err := f.do(action, true); err != nil {
				t.Fatalf("%s/%s: own unfinished setup blocked the command: %v", point, action, err)
			}
			if report, _ := f.do("inspect", false); report.ManagedState != want || report.OperationPending {
				t.Fatalf("%s/%s: own unfinished setup left %+v", point, action, report)
			}
		}
	}
}

// plantOwnSetup leaves Zalki's own first setup stopped at a crash point.
func plantOwnSetup(t *testing.T, f *loginFixture, point string) {
	t.Helper()
	desired, err := desiredLoginItem(f.paths, f.binary, nil)
	if err != nil {
		t.Fatal(err)
	}
	mkdir(t, filepath.Dir(f.paths.lock))
	if err := writePrivateJSON(f.paths.pending, loginItemPending{Schema: 1, Operation: strings.Repeat("b", 32), Action: "install", Phase: "prepared", Desired: desired}); err != nil {
		t.Fatal(err)
	}
	if point != "pending only" {
		writeFile(t, f.paths.plist, renderLoginItem(f.binary, f.state))
	}
	if point == "record committed" {
		if err := writePrivateJSON(f.paths.record, desired); err != nil {
			t.Fatal(err)
		}
	}
}

// Q5: under its own pending setup, other plist bytes are foreign and kept.
func TestPluginLoginItemOwnSetupKeepsOtherPlist(t *testing.T) {
	f := newLoginFixture(t)
	plantOwnSetup(t, f, "pending only")
	writeFile(t, f.paths.plist, []byte("changed"))
	before := snapshotFiles(t, f.paths)
	if _, err := f.do("install", true); !errors.Is(err, errLoginItemForeign) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
		t.Fatalf("other plist bytes under own pending were replaced: %v %v", err, f.fake.mutations())
	}
}

// Q5: an unknown launchd answer keeps the operation pending and blocks new
// starts; a later run takes it over with its OWN token (phase recovering)
// and reconciles only from a fresh look at the job.
func TestPluginLoginItemUnknownAnswerKeepsOperation(t *testing.T) {
	installed := func(f *loginFixture) {
		if _, err := f.do("install", true); err != nil {
			t.Fatal(err)
		}
	}
	// An ordered list (not a map): the first failing case, and so its
	// failing assertion, is the same in every run.
	for _, tc := range []struct {
		name         string
		setup        func(f *loginFixture)
		action, verb string
	}{
		{"first setup bootstrap", func(*loginFixture) {}, "install", "bootstrap"},
		{"owned bootstrap", func(f *loginFixture) { installed(f); f.fake.loaded = false }, "install", "bootstrap"},
		{"owned kickstart", installed, "upgrade", "kickstart"},
		{"uninstall bootout", installed, "uninstall", "bootout"},
	} {
		f := newLoginFixture(t)
		tc.setup(f)
		f.fake.unknownVerb = tc.verb
		if _, err := f.do(tc.action, true); err == nil {
			t.Fatalf("%s: an unknown launchd answer was reported as done", tc.name)
		}
		checkTakeover(t, f, tc.name, tc.action)
	}
}

// checkTakeover runs the recovery of an operation left by an unknown
// answer: first with the job unidentifiable (it must stop with the
// operation kept under its own token), then with launchd answering.
func checkTakeover(t *testing.T, f *loginFixture, name, action string) {
	t.Helper()
	var first loginItemPending
	if ok, err := readPrivateJSON(f.paths.pending, &first); !ok || err != nil {
		t.Fatalf("%s: an unknown launchd answer dropped the pending operation: %v", name, err)
	}
	f.fake.printFails, f.fake.domainFails = true, true
	if _, err := f.do(action, true); err == nil {
		t.Fatalf("%s: recovery acted without identifying the job", name)
	}
	var second loginItemPending
	if ok, err := readPrivateJSON(f.paths.pending, &second); !ok || err != nil {
		t.Fatalf("%s: recovery dropped the operation without identifying the job: %v", name, err)
	}
	if second.Operation == first.Operation || second.Phase != "recovering" {
		t.Fatalf("%s: recovery did not take the operation over with its own token: %+v", name, second)
	}
	f.fake.unknownVerb, f.fake.printFails, f.fake.domainFails = "", false, false
	if _, err := f.do(action, true); err != nil {
		t.Fatalf("%s: the reconciled operation failed: %v", name, err)
	}
	if report, _ := f.do("inspect", false); report.OperationPending {
		t.Fatalf("%s: a reconciled operation stayed pending", name)
	}
}

// S23: a resumed uninstall (its pending record is already there) also
// stops only the owned job.
func TestPluginLoginItemResumedUninstallLeavesForeignJob(t *testing.T) {
	r := newLoginFixture(t)
	if _, err := r.do("install", true); err != nil {
		t.Fatal(err)
	}
	r.fake.bootoutFails = true
	if _, err := r.do("uninstall", true); err == nil {
		t.Fatal("the first uninstall should fail at the stop")
	}
	if _, err := os.Lstat(r.paths.pending); err != nil {
		t.Fatalf("the failed uninstall should keep its pending record: %v", err)
	}
	r.fake.bootoutFails, r.fake.program, r.fake.calls = false, "/tmp/other", nil
	var first loginItemPending
	if _, err := readPrivateJSON(r.paths.pending, &first); err != nil {
		t.Fatal(err)
	}
	plist, _ := os.ReadFile(r.paths.plist)
	record, _ := os.ReadFile(r.paths.record)
	_, err := r.do("uninstall", true)
	// The resumed run takes the operation over (its own token), and keeps
	// the same uninstall of the same prior item.
	var after loginItemPending
	_, pendingErr := readPrivateJSON(r.paths.pending, &after)
	plistAfter, _ := os.ReadFile(r.paths.plist)
	recordAfter, _ := os.ReadFile(r.paths.record)
	if err == nil || slices.Contains(r.fake.mutations(), "bootout") || string(plistAfter) != string(plist) || string(recordAfter) != string(record) ||
		pendingErr != nil || after.Action != "uninstall" || *after.Prior != *first.Prior {
		t.Fatalf("a resumed uninstall booted out a different loaded job or changed the owned files: %v %v", err, r.fake.mutations())
	}
}

// Q5: the job's identity is checked again right before bootout: a job that
// changed after the first look is never stopped.
func TestPluginLoginItemRechecksIdentityBeforeBootout(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	prints := 0
	f.fake.onPrint = func() {
		if prints++; prints == 2 { // the fresh look right before bootout
			f.fake.program = "/tmp/other"
		}
	}
	f.fake.calls = nil
	if _, err := f.do("uninstall", true); err == nil || slices.Contains(f.fake.mutations(), "bootout") {
		t.Fatalf("a job that changed after the first look was stopped: %v %v", err, f.fake.mutations())
	}
}

// replacePending writes another operation's pending record (a takeover) and
// returns its exact bytes.
func replacePending(t *testing.T, f *loginFixture) []byte {
	t.Helper()
	var pending loginItemPending
	if _, err := readPrivateJSON(f.paths.pending, &pending); err != nil {
		t.Fatal(err)
	}
	pending.Operation = strings.Repeat("e", 32)
	if err := writePrivateJSON(f.paths.pending, pending); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(f.paths.pending)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Q5: an operation taken over before or during its launchd change never
// commits or clears: the other operation's pending record stays exactly as
// written, and nothing is started after the takeover.
func TestPluginLoginItemTokenLossStopsCommit(t *testing.T) {
	for _, tc := range []struct {
		name, action, verb string
		owned, loaded      bool
		during             bool // in the launchd call itself, else at the fresh look before it
	}{
		{"setup before bootstrap", "install", "bootstrap", false, false, false},
		{"setup during bootstrap", "install", "bootstrap", false, false, true},
		{"owned bootstrap during", "install", "bootstrap", true, false, true},
		{"owned kickstart during", "upgrade", "kickstart", true, true, true},
	} {
		f := newLoginFixture(t)
		if tc.owned {
			if _, err := f.do("install", true); err != nil {
				t.Fatal(err)
			}
			f.fake.loaded = tc.loaded
		}
		record, _ := os.ReadFile(f.paths.record)
		foreign := takeOverAt(t, f, tc.verb, tc.during)
		f.fake.calls = nil
		_, err := f.do(tc.action, true)
		pending, _ := os.ReadFile(f.paths.pending)
		recordAfter, _ := os.ReadFile(f.paths.record)
		started := slices.Contains(f.fake.mutations(), tc.verb)
		if !errors.Is(err, errLoginItemTakenOver) || *foreign == nil || string(pending) != string(*foreign) || string(recordAfter) != string(record) || (!tc.during && started) {
			t.Errorf("%s: a taken-over operation committed, cleared or started: %v %v", tc.name, err, f.fake.mutations())
		}
	}
}

// M4: an unreadable disabled list never allows a start, even when the
// loaded job matches the owned record.
func TestPluginLoginItemUnknownDisablementBlocksStart(t *testing.T) {
	for _, tc := range []struct {
		name, action string
		set          func(*fakeLaunchd)
	}{
		{"install, query failed", "install", func(l *fakeLaunchd) { l.disabledFails = true }},
		{"install, list unknown", "install", func(l *fakeLaunchd) { l.disabledMalformed = true }},
		{"upgrade, query failed", "upgrade", func(l *fakeLaunchd) { l.disabledFails = true }},
	} {
		f := newLoginFixture(t)
		if _, err := f.do("install", true); err != nil {
			t.Fatal(err)
		}
		tc.set(f.fake)
		f.fake.calls = nil
		_, err := f.do(tc.action, true)
		if len(f.fake.mutations()) != 0 || (tc.action == "install" && !errors.Is(err, errLoginItemUnknown)) {
			t.Errorf("%s: a start ran while the disablement was unknown: %v %v", tc.name, err, f.fake.mutations())
		}
	}
}

// M4: the fresh look right before a start rechecks the disablement: a list
// that turns unreadable after the plan stops the kickstart (owned) or the
// bootstrap (first setup).
func TestPluginLoginItemFreshLookRechecksDisablement(t *testing.T) {
	for _, owned := range []bool{true, false} {
		f := newLoginFixture(t)
		if owned {
			if _, err := f.do("install", true); err != nil {
				t.Fatal(err)
			}
		}
		f.fake.disabledCalls, f.fake.disabledFailsAfter, f.fake.calls = 0, 1, nil
		_, err := f.do("install", true)
		want := errLoginItemUnknown // the owned start clears its own pending record
		if !owned {
			want = errUnfinishedUnknown // the first setup already wrote its files
		}
		if !errors.Is(err, want) || slices.Contains(f.fake.mutations(), "kickstart") || slices.Contains(f.fake.mutations(), "bootstrap") {
			t.Errorf("owned=%v: a start ran after the disablement turned unknown: %v %v", owned, err, f.fake.mutations())
		}
	}
}

// Q5: a takeover while recovery reconciles never lets recovery commit: the
// other operation's pending record stays exactly as written.
func TestPluginLoginItemRecoveryTakeoverStopsCommit(t *testing.T) {
	f := newLoginFixture(t)
	plantOwnSetup(t, f, "pending only")
	var foreign []byte
	f.fake.onPrint = func() {
		if foreign == nil { // recovery's fresh look
			foreign = replacePending(t, f)
		}
	}
	_, err := f.do("install", true)
	pending, _ := os.ReadFile(f.paths.pending)
	_, recordErr := os.Lstat(f.paths.record)
	_, plistErr := os.Lstat(f.paths.plist)
	if !errors.Is(err, errLoginItemTakenOver) || string(pending) != string(foreign) || !errors.Is(recordErr, os.ErrNotExist) || !errors.Is(plistErr, os.ErrNotExist) || len(f.fake.mutations()) != 0 {
		t.Fatalf("recovery wrote or committed after a takeover: %v %v", err, f.fake.mutations())
	}
}

// Q5: the final fence checks the exact owned files, not only the token: a
// plist or record changed during the launchd call stops the commit or the
// clear, and the operation stays pending.
func TestPluginLoginItemFinalFenceChecksFiles(t *testing.T) {
	f := newLoginFixture(t)
	f.fake.onMutate = func(verb string) {
		if verb == "bootstrap" {
			writeFile(t, f.paths.plist, []byte("changed"))
		}
	}
	_, err := f.do("install", true)
	_, recordErr := os.Lstat(f.paths.record)
	_, pendingErr := os.Lstat(f.paths.pending)
	if !errors.Is(err, errUnfinishedChanged) || !errors.Is(recordErr, os.ErrNotExist) || pendingErr != nil {
		t.Fatalf("setup: a commit ran over a changed plist: %v %v %v", err, recordErr, pendingErr)
	}

	g := newLoginFixture(t)
	if _, err := g.do("install", true); err != nil {
		t.Fatal(err)
	}
	g.fake.onMutate = func(verb string) {
		if verb == "kickstart" {
			var record loginItemRecord
			if _, err := readPrivateJSON(g.paths.record, &record); err != nil {
				t.Error(err)
				return
			}
			record.Generation++
			if err := writePrivateJSON(g.paths.record, record); err != nil {
				t.Error(err)
			}
		}
	}
	_, err = g.do("upgrade", true)
	if _, pendingErr := os.Lstat(g.paths.pending); !errors.Is(err, errUnfinishedChanged) || pendingErr != nil {
		t.Fatalf("start: a clear ran over a changed record: %v %v", err, pendingErr)
	}
}

// Q5: the generation never wraps: at its limit a setup is refused before any
// write and the prior state is kept.
func TestPluginLoginItemGenerationCannotOverflow(t *testing.T) {
	f := newLoginFixture(t)
	if _, err := f.do("install", true); err != nil {
		t.Fatal(err)
	}
	var record loginItemRecord
	if _, err := readPrivateJSON(f.paths.record, &record); err != nil {
		t.Fatal(err)
	}
	record.Generation = math.MaxUint64
	if err := writePrivateJSON(f.paths.record, record); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.paths.plist); err != nil {
		t.Fatal(err)
	}
	f.fake.loaded, f.fake.calls = false, nil
	before := snapshotFiles(t, f.paths)
	if _, err := f.do("install", true); !errors.Is(err, errLoginItemGeneration) || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
		t.Fatalf("a setup at the generation limit wrote something: %v %v", err, f.fake.mutations())
	}
}

// C27: an install for another launcher never finishes this launcher's own
// unfinished setup: it is named and nothing is written.
func TestPluginLoginItemOwnSetupForAnotherLauncherIsKept(t *testing.T) {
	for _, point := range []string{"pending only", "plist written"} {
		f := newLoginFixture(t)
		plantOwnSetup(t, f, point)
		old := f.binary
		f.binary = "/usr/local/bin/zalki"
		before := snapshotFiles(t, f.paths)
		_, err := f.do("install", true)
		want := "The zalki login item runs " + old + ", not " + f.binary + "; nothing was changed."
		if err == nil || err.Error() != want || len(f.fake.mutations()) != 0 || snapshotFiles(t, f.paths) != before {
			t.Errorf("%s: another launcher finished this setup: %v %v", point, err, f.fake.mutations())
		}
	}
}

// fakeLaunchctlScript writes an owned executable that stands in for
// launchctl; the real service manager is never run.
func fakeLaunchctlScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "launchctl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// Q5 (real adapter): a deadline or a signal that ends the command is an
// UNKNOWN result, never a known exit status; a normal non-zero exit is known.
func TestPluginLoginItemAdapterKeepsUnknownExits(t *testing.T) {
	old := launchctlPath
	t.Cleanup(func() { launchctlPath = old })
	for _, tc := range []struct {
		name, body string
		wantKnown  bool
		limit      time.Duration // only the deadline case is short
	}{
		{"deadline", "/bin/sleep 5", false, 300 * time.Millisecond},
		{"signal", "kill -9 $$", false, 10 * time.Second},
		{"exit 3", "exit 3", true, 10 * time.Second},
	} {
		launchctlPath = fakeLaunchctlScript(t, tc.body)
		ctx, cancel := context.WithTimeout(context.Background(), tc.limit)
		start := time.Now()
		known, err := runMutationKnown(ctx, "bootstrap", "gui/0", "/nonexistent")
		elapsed := time.Since(start)
		cancel()
		if known != tc.wantKnown || err == nil {
			t.Errorf("%s: an ended launchctl read as known=%v: %v", tc.name, known, err)
		}
		// Bounded: the deadline case returns within its limit plus WaitDelay,
		// even though its sleeping child outlives the killed script.
		if elapsed > tc.limit+3*time.Second {
			t.Errorf("%s: an ended launchctl was not bounded: %v", tc.name, elapsed)
		}
	}

	// Through a whole first setup: the signal-ended bootstrap keeps the
	// pending record, and nothing is committed.
	f := newLoginFixture(t)
	launchctlPath = fakeLaunchctlScript(t, "kill -9 $$")
	runLaunchctl = func(ctx context.Context, args ...string) ([]byte, int, error) {
		if args[0] == "bootstrap" {
			return realLaunchctl(ctx, args...)
		}
		return f.fake.run(ctx, args...)
	}
	_, err := f.do("install", true)
	_, pendingErr := os.Lstat(f.paths.pending)
	_, recordErr := os.Lstat(f.paths.record)
	if err == nil || pendingErr != nil || !errors.Is(recordErr, os.ErrNotExist) {
		t.Fatalf("a signal-ended bootstrap was treated as a known answer: %v %v %v", err, pendingErr, recordErr)
	}
}

// Q5: the files are checked again right before a change: a plist or a
// committed record changed after the plan stops the bootout.
func TestPluginLoginItemFilesRecheckedBeforeChange(t *testing.T) {
	for name, change := range map[string]func(f *loginFixture){
		"plist": func(f *loginFixture) { writeFile(t, f.paths.plist, []byte("changed")) },
		"record": func(f *loginFixture) {
			var record loginItemRecord
			if _, err := readPrivateJSON(f.paths.record, &record); err != nil {
				t.Error(err)
				return
			}
			record.Generation++
			if err := writePrivateJSON(f.paths.record, record); err != nil {
				t.Error(err)
			}
		},
	} {
		f := newLoginFixture(t)
		if _, err := f.do("install", true); err != nil {
			t.Fatal(err)
		}
		prints := 0
		f.fake.onPrint = func() {
			if prints++; prints == 2 { // the fresh look right before bootout
				change(f)
			}
		}
		f.fake.calls = nil
		if _, err := f.do("uninstall", true); !errors.Is(err, errUnfinishedChanged) || slices.Contains(f.fake.mutations(), "bootout") {
			t.Errorf("%s: a bootout ran after the files changed: %v %v", name, err, f.fake.mutations())
		}
	}
}

// takeOverAt arranges another operation's takeover: in the launchd call
// itself (during), or at the fresh look right before it. The returned
// pointer holds the other pending record's exact bytes once written.
func takeOverAt(t *testing.T, f *loginFixture, verb string, during bool) *[]byte {
	t.Helper()
	var foreign []byte
	if during {
		f.fake.onMutate = func(v string) {
			if v == verb && foreign == nil {
				foreign = replacePending(t, f)
			}
		}
		return &foreign
	}
	prints := 0
	f.fake.onPrint = func() {
		if prints++; prints == 2 && foreign == nil {
			foreign = replacePending(t, f)
		}
	}
	return &foreign
}

// Q5: recovery looks at the job BEFORE it repairs any file: with only its
// pending record left, a foreign or unknown loaded job is refused and no
// plist is written.
func TestPluginLoginItemRecoveryLooksBeforeRepair(t *testing.T) {
	for name, set := range map[string]func(*fakeLaunchd){
		"foreign loaded": func(l *fakeLaunchd) { l.loaded, l.program = true, "/tmp/other" },
		"unknown":        func(l *fakeLaunchd) { l.printFails, l.domainFails = true, true },
	} {
		f := newLoginFixture(t)
		plantOwnSetup(t, f, "pending only")
		set(f.fake)
		_, err := f.do("install", true)
		if _, plistErr := os.Lstat(f.paths.plist); err == nil || !errors.Is(plistErr, os.ErrNotExist) || len(f.fake.mutations()) != 0 {
			t.Errorf("%s: recovery wrote the plist before refusing the job: %v %v", name, err, f.fake.mutations())
		}
	}
}

// Q5: a takeover right after the record is published never lets this
// operation remove the other operation's pending record.
func TestPluginLoginItemPublishTakeoverKeepsOtherPending(t *testing.T) {
	f := newLoginFixture(t)
	var foreign []byte
	old := afterRecordPublished
	t.Cleanup(func() { afterRecordPublished = old })
	afterRecordPublished = func() { foreign = replacePending(t, f) }
	_, err := f.do("install", true)
	pending, _ := os.ReadFile(f.paths.pending)
	if !errors.Is(err, errLoginItemTakenOver) || foreign == nil || string(pending) != string(foreign) {
		t.Fatalf("a commit removed another operation's pending record: %v", err)
	}
}

// Q5 (real adapter, bounded): a command that ends but leaves a descendant
// holding its output returns UNKNOWN within the bound, not when the
// descendant lets go.
func TestPluginLoginItemAdapterBoundsHeldOutput(t *testing.T) {
	old := launchctlPath
	t.Cleanup(func() { launchctlPath = old })
	launchctlPath = fakeLaunchctlScript(t, "/bin/sleep 4 &\nexit 0")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	known, err := runMutationKnown(ctx, "bootstrap", "gui/0", "/nonexistent")
	if elapsed := time.Since(start); known || err == nil || elapsed > 3*time.Second {
		t.Fatalf("held launchctl output was not bounded: known=%v err=%v after %v", known, err, elapsed)
	}
}

// Q5 final fence: after a known launchd answer the job is looked at again;
// a job that became different or unreadable during the call keeps the
// operation pending, and nothing is committed or cleared.
func TestPluginLoginItemFinalLookAfterChange(t *testing.T) {
	for _, tc := range []struct {
		name, action, verb string
		owned              bool
		change             func(l *fakeLaunchd)
	}{
		{"setup, other program", "install", "bootstrap", false, func(l *fakeLaunchd) { l.program = "/tmp/other" }},
		{"setup, opaque", "install", "bootstrap", false, func(l *fakeLaunchd) { l.printFails, l.domainFails = true, true }},
		{"kickstart, other program", "upgrade", "kickstart", true, func(l *fakeLaunchd) { l.program = "/tmp/other" }},
		{"kickstart, opaque", "upgrade", "kickstart", true, func(l *fakeLaunchd) { l.printFails, l.domainFails = true, true }},
	} {
		f := newLoginFixture(t)
		if tc.owned {
			if _, err := f.do("install", true); err != nil {
				t.Fatal(err)
			}
		}
		record, _ := os.ReadFile(f.paths.record)
		f.fake.onMutate = func(verb string) {
			if verb == tc.verb {
				tc.change(f.fake)
			}
		}
		_, err := f.do(tc.action, true)
		_, pendingErr := os.Lstat(f.paths.pending)
		recordAfter, _ := os.ReadFile(f.paths.record)
		if err == nil || pendingErr != nil || string(recordAfter) != string(record) || !strings.Contains(err.Error(), "unfinished") {
			t.Errorf("%s: a changed job after the call ended the operation: %v %v", tc.name, err, pendingErr)
		}
	}
}

// Copy: once a first setup has written its files, a stop never says that
// nothing (or the login item) was not changed: it says unfinished.
func TestPluginLoginItemNoFalseNoChangeAfterWrite(t *testing.T) {
	f := newLoginFixture(t)
	f.fake.disabledCalls, f.fake.disabledFailsAfter = 0, 1 // the fresh look after the plist write
	_, err := f.do("install", true)
	if err == nil || strings.Contains(err.Error(), "not changed") || strings.Contains(err.Error(), "nothing was changed") || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("a partial setup claimed no change: %v", err)
	}
}
