package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
	"github.com/zalkilabs/zalki/internal/plugins"
)

// fakeLoginItems stands in for install.ManagedLoginItem: inspect answers
// come from a list (the last one repeats); every change is recorded and
// nothing reaches launchctl or a real login item.
type fakeLoginItems struct {
	reports  []install.Report
	inspects int
	changes  []string
	requests []install.Request
	failWith error
}

func (f *fakeLoginItems) call(_ context.Context, r install.Request) (install.Report, error) {
	f.requests = append(f.requests, r)
	if !r.Apply || r.Action == "inspect" {
		i := min(f.inspects, len(f.reports)-1)
		f.inspects++
		return f.reports[i], nil
	}
	f.changes = append(f.changes, r.Action)
	return install.Report{}, f.failWith
}

// defaultHome makes a fresh account home with the default state and config
// paths selected, and routes login-item calls to a fake.
func defaultHome(t *testing.T, reports ...install.Report) (string, *fakeLoginItems) {
	t.Helper()
	home := t.TempDir()
	for _, name := range []string{"ZALKI_STATE", "XDG_STATE_HOME", "ZALKI_CONFIG", "XDG_CONFIG_HOME"} {
		t.Setenv(name, "")
	}
	fake := &fakeLoginItems{reports: reports}
	previousItems, previousHome := loginItems, loginItemAccountHome
	loginItems, loginItemAccountHome = fake.call, func() string { return home }
	t.Cleanup(func() { loginItems, loginItemAccountHome = previousItems, previousHome })
	return home, fake
}

func defaultState(home string) string { return filepath.Join(home, ".local", "state", "zalki") }

// runDefault runs zalki with the default selectors in home.
func runDefault(t *testing.T, home string, mode installChoice, input string, args ...string) (string, error) {
	t.Helper()
	previous := installTerminal
	installTerminal = func(io.Reader) (installChoice, func()) { return mode, func() {} }
	defer func() { installTerminal = previous }()
	cmd := newRootCommand(home)
	output := &cappedOutput{limit: 4 << 20}
	cmd.SetIn(strings.NewReader(input))
	cmd.SetOut(output)
	cmd.SetErr(output)
	cmd.SetArgs(append([]string{"--log-level", "off"}, args...))
	err := cmd.ExecuteContext(t.Context())
	return output.String(), err
}

func thisBinary(t *testing.T) string {
	t.Helper()
	binary, err := installationBinary()
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

const (
	otherLauncher = "/opt/homebrew/bin/zalki-old"
	// unknownStatusSentence is the 7c owner sentence for an unknown job.
	unknownStatusSentence = "Background plugin status is unknown; the login item was not changed."
)

var (
	reportNone     = install.Report{ManagedState: "missing", ServiceState: "not_loaded", LoginItemRecord: "none"}
	reportDisabled = install.Report{ManagedState: "managed", ServiceState: "disabled"}
)

func reportOwned(launcher string) install.Report {
	return install.Report{ManagedState: "managed", ServiceState: "loaded", LoadedIdentity: "matches", LoginItemLauncher: launcher, LoginItemRecord: shownRecord}
}

// shownRecord is the record token the fake inspect shows for an owned item.
var shownRecord = "1:" + strings.Repeat("a", 64)

// assertBound: every change carries the record token the owner was shown.
func assertBound(t *testing.T, fake *fakeLoginItems, want string) {
	t.Helper()
	for _, r := range fake.requests {
		if r.Apply && r.Action != "inspect" && r.ExpectRecord != want {
			t.Fatalf("a %s was not bound to the shown record: %q, want %q", r.Action, r.ExpectRecord, want)
		}
	}
}

// Every owner line is exact; a change of words is a reviewed change.
func TestPluginLoginItemOwnerLines(t *testing.T) {
	for _, tc := range []struct{ name, got, want string }{
		{"consent", loginItemConsentLine, "It starts now and at every login, and runs until you disable it."},
		{"first item", loginItemFirstLine, "This adds one macOS background item; turn it off any time in System Settings > General > Login Items & Extensions > Allow in the Background."},
		{"combined question", fmt.Sprintf(loginItemCombinedAsk, "B1", "B2"), "Install it, change the zalki login item from B1 to B2, and restart background plugins? [y/N]"},
		{"standalone question", fmt.Sprintf(loginItemRelaunchAsk, "B1", "B2"), "The zalki login item runs B1. Change it to B2 and restart background plugins? [y/N]"},
		{"refusal after build", fmt.Sprintf(loginItemChangedRefusal, "NAME", "REASON"), "Nothing was installed: the zalki login item changed while NAME was being built (REASON). Run the install again."},
		{"not changed", fmt.Sprintf(loginItemNotChangedLine, "REASON"), "Background setup was not changed (REASON); you can run zalki plugins supervise."},
		{"disabled", loginItemDisabledLine, "Background plugins are disabled for dev.zalki.plugins. Check System Settings > General > Login Items & Extensions > Allow in the Background."},
		{"D1 idle", loginItemIdleLine, "The zalki login item stays installed and idle. Remove it with zalki uninstall --apply."},
		{"start requested", fmt.Sprintf(loginItemStartedLine, "NAME", "NAME"), "Installed and enabled NAME. Start requested. Status: zalki plugins status NAME"},
		{"partial", fmt.Sprintf(loginItemPartialLine, "NAME", "REASON", "DIR"), "Installed and enabled NAME. Background setup failed: REASON. Run zalki install --apply in your own terminal to review the login item, or run zalki plugins supervise --state DIR."},
		{"yes not set up", loginItemNotSetUpLine, "Installed and enabled. Background plugins are not set up yet; run zalki plugins install once in your own terminal."},
		{"root question", loginItemRootAsk, "Set up background plugins now? [y/N]"},
		{"root not set up", loginItemRootNotSetUp, "Background plugins are not set up yet; run zalki install --apply in your own terminal."},
		{"root started", loginItemRootStarted, "Background plugins are set up. Start requested."},
		{"relaunched", fmt.Sprintf(loginItemRelaunchedLine, "B2"), "The zalki login item now runs B2. Start requested."},
		{"root failed", fmt.Sprintf(loginItemRootFailed, "REASON"), "Background setup failed: REASON."},
		{"removed note", loginItemRemovedNote, "The zalki login item was removed."},
		{"would remove note", loginItemWouldRemoveNote, "The zalki login item would be removed."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Fatalf("got %q\nwant %q", tc.got, tc.want)
			}
		})
	}
}

// The truth rule: no login-item line promises that a plugin runs or does
// not run; only "Start requested" after a made request, and the consent
// line before the owner agrees.
func TestPluginLoginItemTruthRule(t *testing.T) {
	lines := []string{loginItemConsentLine, loginItemFirstLine, loginItemChangedRefusal, loginItemNotChangedLine,
		loginItemDisabledLine, loginItemIdleLine, loginItemStartedLine, loginItemPartialLine, loginItemNotSetUpLine,
		loginItemRootNotSetUp, loginItemRootStarted, loginItemRelaunchedLine, loginItemRootFailed, loginItemRemovedNote, loginItemWouldRemoveNote}
	for _, line := range lines {
		for _, promise := range []string{"is running", "will run", "will not run", "never runs", "won't run", "stopped", "is safe"} {
			if strings.Contains(line, promise) {
				t.Errorf("line %q promises %q", line, promise)
			}
		}
	}
	// The refused rows install NAME enabled: their lines speak only of the item.
	for _, row := range []loginItemPlan{{row: rowRefused, reason: "its status is unknown"}, {row: rowDisabled}} {
		for _, line := range row.disclosure() {
			if strings.Contains(line, "NAME") || strings.Contains(line, "plugin will") {
				t.Errorf("refused row line %q speaks about the plugin", line)
			}
		}
	}
}

// The consent: one question carries the lines; cancel changes nothing;
// --yes never discloses, asks or changes; extended access with --yes is refused.
func TestPluginLoginItemConsentCLI(t *testing.T) {
	t.Run("first item, yes", consentFirstItemYes)
	t.Run("cancel", consentCancel)
	t.Run("yes flag", consentYesFlag)
	t.Run("yes flag, owned item", consentYesFlagOwnedItem)
	t.Run("extended access with yes", consentExtendedAccessYes)
}

// assertRowChange: a row with no change keeps the slice4 line; a changing
// row makes exactly its action and says "Start requested".
func assertRowChange(t *testing.T, output string, changes []string, change string) {
	t.Helper()
	if change == "" {
		// A refused row is not changed and not kickstarted (R7-H2/M3).
		if len(changes) != 0 || !strings.Contains(output, fmt.Sprintf(wakeInstallUnknown, "demo", "demo")) {
			t.Fatalf("a refused row changed the item: %v\n%s", changes, output)
		}
		return
	}
	if !slices.Equal(changes, []string{change}) || !strings.Contains(output, fmt.Sprintf(loginItemStartedLine, "demo", "demo")) {
		t.Fatalf("row change %v, want %s\n%s", changes, change, output)
	}
}

func consentFirstItemYes(t *testing.T) {
	home, fake := defaultHome(t, reportNone)
	output, err := runDefault(t, home, installRawKeys, "y", "plugins", "install", nativeInstallFolder(t))
	if err != nil {
		t.Fatal(output, err)
	}
	consent, first, question := strings.Index(output, loginItemConsentLine), strings.Index(output, loginItemFirstLine), strings.Index(output, pluginReviewQuestion)
	if consent < 0 || first != consent+len(loginItemConsentLine)+1 || question != first+len(loginItemFirstLine)+1 || strings.Count(output, "[y/N]") != 1 {
		t.Fatalf("the consent lines are not right before the one question:\n%s", output)
	}
	if !slices.Equal(fake.changes, []string{"install"}) || !strings.Contains(output, fmt.Sprintf(loginItemStartedLine, "demo", "demo")) {
		t.Fatalf("yes did not set up the item once: %v\n%s", fake.changes, output)
	}
	assertBound(t, fake, "none")
	for _, r := range fake.requests {
		if r.HomeDir != home || r.StateDir != defaultState(home) || r.Binary != thisBinary(t) {
			t.Fatalf("a request left the default installation: %+v", r)
		}
	}
}

func consentCancel(t *testing.T) {
	home, fake := defaultHome(t, reportNone)
	output, err := runDefault(t, home, installRawKeys, "n", "plugins", "install", nativeInstallFolder(t))
	if err != nil || len(fake.changes) != 0 || !strings.Contains(output, "Nothing was installed") {
		t.Fatalf("cancel changed something: %v %v\n%s", err, fake.changes, output)
	}
	assertNothingInstalled(t, defaultState(home))
}

func consentYesFlag(t *testing.T) {
	home, fake := defaultHome(t, reportNone)
	folder := nativeInstallFolder(t)
	output, err := runDefault(t, home, installNoTerminal, "", "plugins", "install", "--yes", "--allow-unsandboxed", "--tree-sha256", treeOf(t, folder), folder)
	if err != nil || len(fake.changes) != 0 || strings.Contains(output, loginItemConsentLine) || strings.Count(output, "[y/N]") != 0 || !strings.Contains(output, loginItemNotSetUpLine) {
		t.Fatalf("--yes touched or offered the login item: %v %v\n%s", err, fake.changes, output)
	}
}

func consentYesFlagOwnedItem(t *testing.T) {
	home, fake := defaultHome(t, reportOwned(thisBinary(t)))
	folder := nativeInstallFolder(t)
	output, err := runDefault(t, home, installNoTerminal, "", "plugins", "install", "--yes", "--allow-unsandboxed", "--tree-sha256", treeOf(t, folder), folder)
	// R7-H2: the owned, loaded item gets the non-killing kickstart only
	// ("upgrade" wakes; it never creates or changes the item).
	if err != nil || !slices.Equal(fake.changes, []string{"upgrade"}) || !strings.Contains(output, fmt.Sprintf(loginItemStartedLine, "demo", "demo")) {
		t.Fatalf("--yes with an owned item did not wake the supervisor: %v %v\n%s", err, fake.changes, output)
	}
}

func consentExtendedAccessYes(t *testing.T) {
	home, fake := defaultHome(t, reportNone)
	folder := writeInstallFolder(t, strings.Replace(nativeInstallManifest, `"send":false`, `"send":true`, 1), map[string]string{"bin/plugin": "#!/bin/sh\nexit 0\n"})
	output, err := runDefault(t, home, installNoTerminal, "", "plugins", "install", "--yes", "--allow-unsandboxed", "--tree-sha256", treeOf(t, folder), folder)
	if err == nil || !strings.Contains(err.Error(), "extended access") || len(fake.changes) != 0 {
		t.Fatalf("extended access with --yes was not refused: %v %v\n%s", err, fake.changes, output)
	}
}

// treeOf is the snapshot hash that --tree-sha256 expects for folder.
func treeOf(t *testing.T, folder string) string {
	t.Helper()
	snapshot, err := plugins.SnapshotInstallSource(folder, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot.TreeSHA256
}

// The decision table: one question in every row, and only the first,
// owned and relaunch rows change the item, each with its own action.
func TestPluginLoginItemDecisionTable(t *testing.T) {
	b2 := thisBinary(t)
	for _, tc := range []struct {
		name     string
		report   install.Report
		lines    []string
		question string
		change   string
	}{
		{"first", reportNone, []string{loginItemConsentLine, loginItemFirstLine}, pluginReviewQuestion, "install"},
		{"owned", reportOwned(b2), []string{loginItemConsentLine}, pluginReviewQuestion, "install"},
		{"owned, not loaded", install.Report{ManagedState: "managed", ServiceState: "not_loaded", LoginItemLauncher: b2}, []string{loginItemConsentLine}, pluginReviewQuestion, "install"},
		{"relaunch", reportOwned(otherLauncher), []string{loginItemConsentLine}, fmt.Sprintf(loginItemCombinedAsk, otherLauncher, b2), "relaunch"},
		{"disabled", reportDisabled, []string{loginItemDisabledLine}, pluginReviewQuestion, ""},
		{"pending", install.Report{ManagedState: "managed", ServiceState: "loaded", LoadedIdentity: "matches", OperationPending: true}, []string{fmt.Sprintf(loginItemNotChangedLine, loginItemPendingReason)}, pluginReviewQuestion, ""},
		{"other job loaded", install.Report{ManagedState: "managed", ServiceState: "loaded", LoadedIdentity: "mismatch", LoginItemLauncher: b2}, []string{fmt.Sprintf(loginItemNotChangedLine, "a different job is loaded")}, pluginReviewQuestion, ""},
		{"unrecorded job", install.Report{ManagedState: "missing", ServiceState: "loaded", LoadedIdentity: "mismatch"}, []string{fmt.Sprintf(loginItemNotChangedLine, "a job is loaded that zalki has no record of")}, pluginReviewQuestion, ""},
		{"foreign", install.Report{ManagedState: "foreign", ServiceState: "not_loaded"}, []string{fmt.Sprintf(loginItemNotChangedLine, "a login item file exists that zalki did not write")}, pluginReviewQuestion, ""},
		{"changed", install.Report{ManagedState: "changed", ServiceState: "not_loaded"}, []string{fmt.Sprintf(loginItemNotChangedLine, "the login item file was changed after zalki wrote it")}, pluginReviewQuestion, ""},
		{"unsafe", install.Report{ManagedState: "unsafe", ServiceState: "not_loaded"}, []string{fmt.Sprintf(loginItemNotChangedLine, "the login item file is a link or not a private file")}, pluginReviewQuestion, ""},
		{"invalid record", install.Report{ManagedState: "invalid_record", ServiceState: "unknown"}, []string{fmt.Sprintf(loginItemNotChangedLine, "the login item record is not valid")}, pluginReviewQuestion, ""},
		{"unknown job", install.Report{ManagedState: "managed", ServiceState: "unknown", LoginItemLauncher: b2}, []string{fmt.Sprintf(loginItemNotChangedLine, "its status is unknown")}, pluginReviewQuestion, ""},
		{"unknown file", install.Report{ManagedState: "unknown", ServiceState: "not_loaded"}, []string{fmt.Sprintf(loginItemNotChangedLine, "its status is unknown")}, pluginReviewQuestion, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, fake := defaultHome(t, tc.report)
			output, err := runDefault(t, home, installRawKeys, "y", "plugins", "install", nativeInstallFolder(t))
			if err != nil {
				t.Fatal(output, err)
			}
			want := strings.Join(tc.lines, "\n") + "\n" + tc.question + " "
			if !strings.Contains(output, want) || strings.Count(output, "[y/N]") != 1 {
				t.Fatalf("row lines/question:\n%s\nwant:\n%s", output, want)
			}
			assertRowChange(t, output, fake.changes, tc.change)
			assertBound(t, fake, tc.report.LoginItemRecord)
		})
	}
	t.Run("no to the combined question", decisionNoToCombinedQuestion)
	t.Run("failed change after enable", decisionFailedChangeAfterEnable)
}

func decisionNoToCombinedQuestion(t *testing.T) {
	home, fake := defaultHome(t, reportOwned(otherLauncher))
	output, err := runDefault(t, home, installRawKeys, "n", "plugins", "install", nativeInstallFolder(t))
	if err != nil || len(fake.changes) != 0 {
		t.Fatalf("no changed something: %v %v\n%s", err, fake.changes, output)
	}
	assertNothingInstalled(t, defaultState(home))
}

func decisionFailedChangeAfterEnable(t *testing.T) {
	home, fake := defaultHome(t, reportNone)
	fake.failWith = errors.New(unknownStatusSentence)
	output, err := runDefault(t, home, installRawKeys, "y", "plugins", "install", nativeInstallFolder(t))
	want := fmt.Sprintf(loginItemPartialLine, "demo", strings.TrimSuffix(unknownStatusSentence, "."), defaultState(home))
	if err == nil || err.Error() != want || strings.Contains(output, "Start requested") {
		t.Fatalf("partial result line: %v\n%s", err, output)
	}
}

// The same read-only look runs after the build; a change of ANY input
// (file state, job state, identity, launcher, pending, a failed read) gives
// one refusal line, nothing published and no second question.
func TestPluginLoginItemRecheckAfterBuild(t *testing.T) {
	b2 := thisBinary(t)
	owned := reportOwned(b2)
	for _, tc := range []struct {
		name   string
		before install.Report
		after  install.Report
		reason string
	}{
		{"managed state", reportNone, install.Report{ManagedState: "foreign", ServiceState: "not_loaded"}, "a login item file exists that zalki did not write"},
		{"service state", owned, install.Report{ManagedState: "managed", ServiceState: "not_loaded", LoginItemLauncher: b2}, "its state changed"},
		{"loaded identity", owned, install.Report{ManagedState: "managed", ServiceState: "loaded", LoadedIdentity: "mismatch", LoginItemLauncher: b2}, "a different job is loaded"},
		{"launcher", owned, reportOwned(otherLauncher), "it now runs " + otherLauncher},
		{"pending", owned, install.Report{ManagedState: "managed", ServiceState: "loaded", LoadedIdentity: "matches", OperationPending: true}, loginItemPendingReason},
		{"disabled", owned, reportDisabled, "it is disabled in System Settings"},
		{"set up meanwhile", reportNone, owned, "it now runs " + b2},
		{"removed meanwhile", owned, reportNone, "it is not set up"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, fake := defaultHome(t, tc.before, tc.after)
			output, err := runDefault(t, home, installRawKeys, "y", "plugins", "install", nativeInstallFolder(t))
			want := fmt.Sprintf(loginItemChangedRefusal, "demo", tc.reason)
			if err == nil || err.Error() != want || len(fake.changes) != 0 || strings.Count(output, "[y/N]") != 1 || fake.inspects != 2 {
				t.Fatalf("recheck: %v (want %q) %v inspects %d\n%s", err, want, fake.changes, fake.inspects, output)
			}
			assertNothingInstalled(t, defaultState(home))
		})
	}
	t.Run("failed read", func(t *testing.T) {
		home, fake := defaultHome(t, reportNone)
		calls := 0
		loginItems = func(ctx context.Context, r install.Request) (install.Report, error) {
			if calls++; calls == 2 {
				return install.Report{}, errors.New("read failed")
			}
			return fake.call(ctx, r)
		}
		_, err := runDefault(t, home, installRawKeys, "y", "plugins", "install", nativeInstallFolder(t))
		if want := fmt.Sprintf(loginItemChangedRefusal, "demo", "its status is unknown"); err == nil || err.Error() != want || len(fake.changes) != 0 {
			t.Fatalf("a failed recheck read: %v %v", err, fake.changes)
		}
	})
}

// Only the default state and config reach the login item; any explicit
// selector (even with the same text) or a scratch HOME never calls it.
func TestPluginLoginItemSelectorProvenance(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, home string) []string
	}{
		{"--state with the default text", func(_ *testing.T, home string) []string { return []string{"--state", defaultState(home)} }},
		{"ZALKI_STATE", func(t *testing.T, home string) []string {
			t.Helper()
			t.Setenv("ZALKI_STATE", defaultState(home))
			return nil
		}},
		{"XDG_STATE_HOME", func(t *testing.T, home string) []string {
			t.Helper()
			t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
			return nil
		}},
		{"--config", func(_ *testing.T, home string) []string {
			return []string{"--config", filepath.Join(home, ".config", "zalki", "config.json")}
		}},
		{"ZALKI_CONFIG", func(t *testing.T, home string) []string {
			t.Helper()
			t.Setenv("ZALKI_CONFIG", filepath.Join(home, ".config", "zalki", "config.json"))
			return nil
		}},
		{"XDG_CONFIG_HOME", func(t *testing.T, home string) []string {
			t.Helper()
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			return nil
		}},
		{"scratch HOME", func(*testing.T, string) []string {
			loginItemAccountHome = func() string { return filepath.Join(t.TempDir(), "other-account") }
			return nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, fake := defaultHome(t, reportNone)
			args := append(tc.setup(t, home), "plugins", "install", nativeInstallFolder(t))
			output, err := runDefault(t, home, installRawKeys, "y", args...)
			if err != nil || len(fake.requests) != 0 || strings.Contains(output, loginItemConsentLine) || !strings.Contains(output, "Installed. Start it with: zalki plugins run demo") {
				t.Fatalf("a non-default selector reached the login item: %v %d\n%s", err, len(fake.requests), output)
			}
		})
	}
}

// rootFixture is a default-state store with one enabled plugin.
func rootFixture(t *testing.T, reports ...install.Report) (*cobra.Command, *loginItemTarget, bus.Store, *fakeLoginItems, *cappedOutput) {
	t.Helper()
	home, fake := defaultHome(t, reports...)
	folder := nativeInstallFolder(t)
	if output, err := pluginInstallLocal(t, defaultState(home), installRawKeys, "y", folder); err != nil {
		t.Fatal(output, err)
	}
	store, err := bus.NewStore(defaultState(home), adapters.Builtin(), nil)
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	output := &cappedOutput{limit: 1 << 20}
	cmd.SetOut(output)
	cmd.SetErr(output)
	return cmd, &loginItemTarget{binary: thisBinary(t), home: home, state: defaultState(home)}, store, fake, output
}

func withTerminal(t *testing.T, mode installChoice) {
	t.Helper()
	previous := installTerminal
	installTerminal = func(io.Reader) (installChoice, func()) { return mode, func() {} }
	t.Cleanup(func() { installTerminal = previous })
}

// checkRootRow runs the root setup for one row and checks its lines, its
// one question, its change and the binding of that change.
func checkRootRow(t *testing.T, report install.Report, mode installChoice, input, want string, change []string, asks int) {
	t.Helper()
	cmd, target, store, fake, output := rootFixture(t, report)
	withTerminal(t, mode)
	cmd.SetIn(strings.NewReader(input))
	if err := target.rootSetup(cmd, store); err != nil {
		t.Fatal(err, output.String())
	}
	if !strings.Contains(output.String(), want) || !slices.Equal(fake.changes, change) || strings.Count(output.String(), "[y/N]") != asks {
		t.Fatalf("root row: %v\n%s\nwant %q", fake.changes, output.String(), want)
	}
	if want == "" && output.Len() != 0 {
		t.Fatalf("a quiet row printed: %s", output.String())
	}
	if !report.OperationPending {
		assertBound(t, fake, report.LoginItemRecord)
	}
}

// Root `zalki install --apply`: the same rows without a build, at most one question.
func TestPluginLoginItemRootInstallReview(t *testing.T) {
	b2 := thisBinary(t)
	for _, tc := range []struct {
		name   string
		report install.Report
		mode   installChoice
		input  string
		want   string
		change []string
		asks   int
	}{
		{"first, yes", reportNone, installRawKeys, "y", loginItemConsentLine + "\n" + loginItemFirstLine + "\n" + loginItemRootAsk + " \n" + loginItemRootStarted, []string{"install"}, 1},
		{"first, no", reportNone, installRawKeys, "n", loginItemRootAsk, nil, 1},
		{"relaunch, yes", reportOwned(otherLauncher), installRawKeys, "y", fmt.Sprintf(loginItemRelaunchAsk, otherLauncher, b2) + " \n" + fmt.Sprintf(loginItemRelaunchedLine, b2), []string{"relaunch"}, 1},
		{"relaunch, no", reportOwned(otherLauncher), installRawKeys, "n", fmt.Sprintf(loginItemRelaunchAsk, otherLauncher, b2), nil, 1},
		{"owned", reportOwned(b2), installRawKeys, "y", "", nil, 0},
		{"owned, not loaded, yes", install.Report{ManagedState: "managed", ServiceState: "not_loaded", LoginItemLauncher: b2, LoginItemRecord: shownRecord}, installRawKeys, "y", loginItemConsentLine + "\n" + loginItemRootAsk + " \n" + loginItemRootStarted, []string{"install"}, 1},
		{"owned, not loaded, no", install.Report{ManagedState: "managed", ServiceState: "not_loaded", LoginItemLauncher: b2, LoginItemRecord: shownRecord}, installRawKeys, "n", loginItemRootAsk, nil, 1},
		{"refused", install.Report{ManagedState: "foreign", ServiceState: "not_loaded"}, installRawKeys, "y", fmt.Sprintf(loginItemNotChangedLine, "a login item file exists that zalki did not write"), nil, 0},
		{"disabled", reportDisabled, installRawKeys, "y", loginItemDisabledLine, nil, 0},
		{"pending resumes without a question", install.Report{ManagedState: "managed", ServiceState: "not_loaded", OperationPending: true}, installRawKeys, "", loginItemRootStarted, []string{"install"}, 0},
		{"no terminal, no item", reportNone, installNoTerminal, "", loginItemRootNotSetUp, nil, 0},
		{"no terminal, owned", reportOwned(b2), installNoTerminal, "", "", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkRootRow(t, tc.report, tc.mode, tc.input, tc.want, tc.change, tc.asks)
		})
	}
	t.Run("no enabled plugin", func(t *testing.T) {
		_, fake := defaultHome(t, reportNone)
		withTerminal(t, installRawKeys)
		store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), adapters.Builtin(), nil)
		if err != nil {
			t.Fatal(err)
		}
		cmd := &cobra.Command{}
		cmd.SetContext(t.Context())
		output := &cappedOutput{limit: 1 << 20}
		cmd.SetOut(output)
		cmd.SetIn(strings.NewReader("y"))
		if err := (&loginItemTarget{binary: "/x", home: "/h", state: store.Dir}).rootSetup(cmd, store); err != nil || len(fake.requests) != 0 || output.Len() != 0 {
			t.Fatalf("root setup without enabled plugins: %v %d %s", err, len(fake.requests), output.String())
		}
	})
	t.Run("failure line", func(t *testing.T) {
		cmd, target, store, fake, _ := rootFixture(t, reportNone)
		withTerminal(t, installRawKeys)
		cmd.SetIn(strings.NewReader("y"))
		fake.failWith = errors.New("a different dev.zalki.plugins job is loaded; nothing was changed")
		err := target.rootSetup(cmd, store)
		if want := fmt.Sprintf(loginItemRootFailed, "a different dev.zalki.plugins job is loaded; nothing was changed"); err == nil || err.Error() != want {
			t.Fatalf("root failure line: %v", err)
		}
	})
}

// Uninstall removes only our item, with no question; the preview only names it.
func TestPluginLoginItemUninstallCLI(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report install.Report
		apply  bool
		note   string
		change []string
	}{
		{"owned, apply", reportOwned("/x/zalki"), true, loginItemRemovedNote, []string{"uninstall"}},
		{"pending, apply", install.Report{ManagedState: "managed", OperationPending: true}, true, loginItemRemovedNote, []string{"uninstall"}},
		{"owned, preview", reportOwned("/x/zalki"), false, loginItemWouldRemoveNote, nil},
		{"foreign, apply", install.Report{ManagedState: "foreign", ServiceState: "not_loaded"}, true, "", nil},
		{"none, apply", reportNone, true, "", nil},
		{"unknown, apply", install.Report{ManagedState: "unknown", ServiceState: "unknown"}, true, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fake := defaultHome(t, tc.report)
			note, err := (&loginItemTarget{binary: "/x/zalki", home: "/h", state: "/h/s"}).uninstallNote(t.Context(), tc.apply)
			if err != nil || note != tc.note || !slices.Equal(fake.changes, tc.change) {
				t.Fatalf("uninstall: %q %v %v", note, err, fake.changes)
			}
		})
	}
	t.Run("owned, job unknown", checkUninstallManualSteps)
	t.Run("command, default selectors", func(t *testing.T) {
		home, fake := defaultHome(t, reportOwned(thisBinary(t)))
		output, err := runDefault(t, home, installNoTerminal, "", "uninstall", "--apply")
		if err != nil || !slices.Equal(fake.changes, []string{"uninstall"}) || !strings.Contains(output, loginItemRemovedNote) || strings.Contains(output, "[y/N]") {
			t.Fatalf("uninstall --apply: %v %v\n%s", err, fake.changes, output)
		}
	})
	t.Run("command, custom state", func(t *testing.T) {
		home, fake := defaultHome(t, reportOwned(thisBinary(t)))
		output, err := runDefault(t, home, installNoTerminal, "", "--state", filepath.Join(home, "other"), "uninstall", "--apply")
		if err != nil || len(fake.requests) != 0 {
			t.Fatalf("a custom-state uninstall reached the login item: %v %d\n%s", err, len(fake.requests), output)
		}
	})
}

// Upgrade and restart start the owned job again only with fresh evidence
// that it is ours, loaded, matching and not disabled; never a question.
func TestPluginLoginItemRestartKickstart(t *testing.T) {
	b2 := thisBinary(t)
	for _, tc := range []struct {
		name   string
		report install.Report
		kicked bool
	}{
		{"owned loaded matching", reportOwned(b2), true},
		{"owned not loaded", install.Report{ManagedState: "managed", ServiceState: "not_loaded", LoginItemLauncher: b2}, false},
		{"foreign", install.Report{ManagedState: "foreign", ServiceState: "loaded", LoadedIdentity: "mismatch"}, false},
		{"changed", install.Report{ManagedState: "changed", ServiceState: "loaded", LoadedIdentity: "matches"}, false},
		{"unknown", install.Report{ManagedState: "managed", ServiceState: "unknown", LoginItemLauncher: b2}, false},
		{"disabled", reportDisabled, false},
		{"other launcher", reportOwned(otherLauncher), false},
		{"pending", install.Report{ManagedState: "managed", ServiceState: "loaded", LoadedIdentity: "matches", OperationPending: true}, false},
		{"other job", install.Report{ManagedState: "managed", ServiceState: "loaded", LoadedIdentity: "mismatch", LoginItemLauncher: b2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, fake := defaultHome(t, tc.report)
			kicked, err := (&loginItemTarget{binary: b2, home: "/h", state: "/h/s"}).kickstart(t.Context())
			want := []string(nil)
			if tc.kicked {
				want = []string{"upgrade"}
			}
			if err != nil || kicked != tc.kicked || !slices.Equal(fake.changes, want) {
				t.Fatalf("kickstart: %v %v %v", kicked, err, fake.changes)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		report install.Report
		want   string
		change []string
	}{
		{"restart, owned job", reportOwned(b2), "Restart requested for demo.\n", []string{"upgrade"}},
		{"restart, foreign job", install.Report{ManagedState: "foreign", ServiceState: "loaded", LoadedIdentity: "mismatch"}, "Run zalki plugins supervise --state ", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, target, _, fake, _ := rootFixture(t, tc.report)
			output, err := runDefault(t, target.home, installNoTerminal, "", "plugins", "restart", "demo")
			if err != nil || !strings.HasSuffix(output, tc.want) && !strings.Contains(output, tc.want) || !slices.Equal(fake.changes, tc.change) || strings.Contains(output, "[y/N]") {
				t.Fatalf("restart: %v %v\n%s", err, fake.changes, output)
			}
			if tc.change != nil && strings.Contains(output, "supervise --state") {
				t.Fatalf("restart printed the manual route after a kickstart: %s", output)
			}
		})
	}
}

// Upgrade success: stdout unchanged, the kickstart only through the gate.
func TestPluginLoginItemUpgradeKickstart(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report install.Report
		state  bool
		change []string
	}{
		{"owned job", reportOwned(""), false, []string{"upgrade"}},
		{"disabled", reportDisabled, false, nil},
		{"custom state", reportOwned(""), true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := tc.report
			if report.LoginItemLauncher == "" && report.ManagedState == "managed" && report.ServiceState == "loaded" {
				report.LoginItemLauncher = thisBinary(t)
			}
			home, fake := defaultHome(t, report)
			t.Setenv("HOME", home)
			for _, variable := range []string{"CODEX_HOME", "CLAUDE_CONFIG_DIR", "CURSOR_CONFIG_DIR"} {
				t.Setenv(variable, "")
			}
			var selector []string
			if tc.state {
				selector = []string{"--state", filepath.Join(home, "custom")}
			}
			if output, err := runDefault(t, home, installNoTerminal, "", append(selector, "install", "--apply")...); err != nil {
				t.Fatal(output, err)
			}
			output, err := runDefault(t, home, installNoTerminal, "", append(selector, "upgrade", "--finish-upgrade")...)
			if err != nil || output != upgradeSuccess+"\n" || !slices.Equal(fake.changes, tc.change) {
				t.Fatalf("upgrade: %v %v %q", err, fake.changes, output)
			}
		})
	}
}

// D1: the last plugin removed while our item stays gives one extra line.
func TestPluginLoginItemLastPluginLine(t *testing.T) {
	_, target, store, _, _ := rootFixture(t, reportOwned("/x/zalki"))
	if line := target.idleLine(t.Context(), store); line != "" {
		t.Fatalf("D1 line with a plugin installed: %q", line)
	}
	if output, err := pluginOwnerLocal(t, store.Dir, "remove", "demo"); err != nil {
		t.Fatal(string(output), err)
	}
	if line := target.idleLine(t.Context(), store); line != loginItemIdleLine {
		t.Fatalf("no D1 line after the last plugin: %q", line)
	}
	loginItems = (&fakeLoginItems{reports: []install.Report{reportNone}}).call
	if line := target.idleLine(t.Context(), store); line != "" {
		t.Fatalf("D1 line without an item: %q", line)
	}
}

// checkUninstallManualSteps (R7-L6): an owned item whose job cannot be
// checked is not removed; the note gives the by-hand route, which confirms
// the job is ours, stops it, and deletes the files only once it is gone.
func checkUninstallManualSteps(t *testing.T) {
	_, fake := defaultHome(t, install.Report{ManagedState: "managed", ServiceState: "unknown", LoginItemLauncher: "/x/zalki"})
	fake.failWith = errors.New("background plugin status is unknown")
	note, err := (&loginItemTarget{binary: "/x/zalki", home: "/h", state: "/h/s"}).uninstallNote(t.Context(), true)
	if err == nil {
		t.Fatal("an unknown job was reported as removed")
	}
	for _, want := range []string{"could not be checked, so it was not removed", "launchctl print gui/", "program /x/zalki with the arguments plugins supervise --state /h/s",
		"launchctl bootout gui/", "says the service is not found, compare shasum -a 256 /h/Library/LaunchAgents/dev.zalki.plugins.plist with \"plist_sha256\" in /h/s/plugins/login-item.installation.json",
		"only when both match, delete the plist and that record; leave /h/s/plugins/login-item.installation.pending.json in place", "then run zalki doctor"} {
		if !strings.Contains(note, want) {
			t.Fatalf("an unknown uninstall gave no manual route (missing %q): %q", want, note)
		}
	}
}
