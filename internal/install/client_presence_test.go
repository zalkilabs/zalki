package install_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// These tests use three made-up clients with the rule every client follows: a
// client is configured only when its configuration directory exists or its
// command is found. "red" keeps a file in .red; "blue" keeps one in .blue that its
// BLUE_HOME variable can move; "green" keeps one in the home directory and one in
// .green. Every file is under a temporary home.

func clientEntry(binary, client string) map[string]any {
	return map[string]any{"Stop": []any{map[string]any{"type": "command", "command": "'" + binary + "' 'hook' '--agent' '" + client + "'", "timeout": 5}}}
}

func threeClients(binary, _ string) ([]install.ConfigFile, error) {
	entry := func(client string) map[string]any { return map[string]any{"zalki": clientEntry(binary, client)} }
	return []install.ConfigFile{
		{Path: ".blue/hooks.json", Draft: "blue.json", JSON: entry("blue"), OwnedEntry: "zalki", ConfigRootEnv: "BLUE_HOME", Client: "blue", ClientLabel: "Blue", Executable: "blue-cmd"},
		{Path: ".green.json", Draft: "green-main.json", JSON: entry("green"), OwnedEntry: "zalki", Client: "green", ClientLabel: "Green", Executable: "green-cmd"},
		{Path: ".green/hooks.json", Draft: "green.json", JSON: entry("green"), OwnedEntry: "zalki", Client: "green", ClientLabel: "Green", Executable: "green-cmd"},
		{Path: ".red/hooks.json", Draft: "red.json", JSON: entry("red"), OwnedEntry: "zalki", Client: "red", ClientLabel: "Red", Executable: "red-cmd"},
	}, nil
}

type clientHome struct {
	t     *testing.T
	home  string
	found map[string]bool // commands that are found
}

func newClientHome(t *testing.T) *clientHome {
	t.Helper()
	isolateConfigEnv(t)
	t.Setenv("BLUE_HOME", "")
	return &clientHome{t: t, home: t.TempDir(), found: map[string]bool{}}
}

func (c *clientHome) request(action, binary string, apply bool) install.Request {
	request := goldenRequest(c.home, action, binary)
	request.Apply = apply
	request.FindExecutable = func(name string) bool { return c.found[name] }
	return request
}

func (c *clientHome) run(action, binary string) install.Report {
	c.t.Helper()
	report, err := install.Manage(threeClients, c.request(action, binary, true))
	if err != nil {
		c.t.Fatalf("%s: %v", action, err)
	}
	return report
}

func (c *clientHome) mkdir(names ...string) {
	c.t.Helper()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Join(c.home, filepath.FromSlash(name)), 0o700); err != nil {
			c.t.Fatal(err)
		}
	}
}

func (c *clientHome) exists(name string) bool {
	_, err := os.Stat(filepath.Join(c.home, filepath.FromSlash(name)))
	return err == nil
}

func (c *clientHome) read(name string) string {
	c.t.Helper()
	raw, err := os.ReadFile(filepath.Join(c.home, filepath.FromSlash(name)))
	if err != nil {
		c.t.Fatal(err)
	}
	return string(raw)
}

func (c *clientHome) write(name, content string) {
	c.t.Helper()
	c.mkdir(filepath.ToSlash(filepath.Dir(filepath.FromSlash(name))))
	if err := os.WriteFile(filepath.Join(c.home, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
		c.t.Fatal(err)
	}
}

// recorded returns the files the installation record lists.
func (c *clientHome) recorded() []string {
	c.t.Helper()
	if !c.exists("state/installation.json") {
		return nil
	}
	var record struct {
		Targets map[string]string `json:"targets"`
	}
	if err := jsonUnmarshal([]byte(c.read("state/installation.json")), &record); err != nil {
		c.t.Fatal(err)
	}
	var names []string
	for name := range record.Targets {
		names = append(names, name)
	}
	sortStrings(names)
	return names
}

func TestInstallRefusesToConfigureNoClient(t *testing.T) {
	c := newClientHome(t)
	for _, action := range []string{"install", "upgrade"} {
		_, err := install.Manage(threeClients, c.request(action, goldenBinary, true))
		want := "No supported client found: Blue, Green, Red. Install one, then run zalki " + action + "."
		if err == nil || err.Error() != want {
			t.Fatalf("%s: error %v, want %q", action, err, want)
		}
	}
	if c.exists(".blue") || c.exists(".green") || c.exists(".red") || c.exists("state") {
		t.Fatal("a refused run wrote something")
	}
}

func TestOnlyTheClientsOnThisMachineAreConfigured(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".red")
	report := c.run("install", goldenBinary)
	if !reflect.DeepEqual(report.Configured, []string{"red"}) || len(report.Unusable) != 0 {
		t.Fatalf("configured %v, unusable %v", report.Configured, report.Unusable)
	}
	if want := []string{"Not found: Blue, Green. Install one, then run zalki install."}; !reflect.DeepEqual(report.Notes, want) {
		t.Fatalf("notes = %q, want %q", report.Notes, want)
	}
	if !c.exists(".red/hooks.json") || c.exists(".blue") || c.exists(".green") || c.exists(".green.json") {
		t.Fatal("a client that is not here was configured, or the present one was not")
	}
	if got := c.recorded(); !reflect.DeepEqual(got, []string{".red/hooks.json"}) {
		t.Fatalf("the record lists %v, want only the red file", got)
	}
	// The same run again changes nothing.
	before := snapshotHome(t, c.home)
	c.run("install", goldenBinary)
	if !equalFiles(before, snapshotHome(t, c.home)) {
		t.Fatal("a second install changed files")
	}
}

func TestACommandThatIsFoundMakesAClientPresent(t *testing.T) {
	c := newClientHome(t)
	c.found["green-cmd"] = true
	report := c.run("install", goldenBinary)
	if !reflect.DeepEqual(report.Configured, []string{"green"}) || !c.exists(".green/hooks.json") || !c.exists(".green.json") {
		t.Fatalf("configured %v; the files of a client whose command is found were not written", report.Configured)
	}
}

// A file directly in the home directory says nothing: that directory always exists.
func TestAFileInTheHomeDirectoryDoesNotMakeAClientPresent(t *testing.T) {
	c := newClientHome(t)
	c.write(".green.json", "{}")
	c.mkdir(".red")
	if report := c.run("install", goldenBinary); !reflect.DeepEqual(report.Configured, []string{"red"}) {
		t.Fatalf("configured %v, want only red", report.Configured)
	}
}

// Presence is checked on every run: a client that appears after the install is configured by the next upgrade.
func TestAClientThatAppearsAfterTheInstallIsConfiguredByTheNextUpgrade(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".red")
	c.run("install", goldenBinary)
	c.mkdir(".blue")
	report := c.run("upgrade", goldenBinary)
	if !reflect.DeepEqual(report.Configured, []string{"blue", "red"}) || !c.exists(".blue/hooks.json") {
		t.Fatalf("configured %v; blue was not added", report.Configured)
	}
	if got := c.recorded(); !reflect.DeepEqual(got, []string{".blue/hooks.json", ".red/hooks.json"}) {
		t.Fatalf("the record lists %v", got)
	}
}

// The directory that counts is the one the client really uses: its variable, when set.
func TestTheEnvironmentVariableOfAClientMovesTheDirectoryThatCounts(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".blue") // exists, but BLUE_HOME points elsewhere
	moved := filepath.Join(t.TempDir(), "elsewhere")
	t.Setenv("BLUE_HOME", moved)
	if _, err := install.Manage(threeClients, c.request("install", goldenBinary, true)); err == nil || !strings.HasPrefix(err.Error(), "No supported client found") {
		t.Fatalf("error %v: a client whose directory does not exist was configured", err)
	}
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	report := c.run("install", goldenBinary)
	if !reflect.DeepEqual(report.Configured, []string{"blue"}) {
		t.Fatalf("configured %v, want blue", report.Configured)
	}
	if _, err := os.Stat(filepath.Join(moved, "hooks.json")); err != nil {
		t.Fatalf("the file was not written where BLUE_HOME points: %v", err)
	}
}

// One file that cannot be used skips its client only; the others are written.
func TestAnUnusableFileSkipsOnlyItsClient(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".blue")
	c.write(".red/hooks.json", `{"a": 1 "b": 2}`)
	report := c.run("install", goldenBinary)
	if !reflect.DeepEqual(report.Unusable, []string{"Red"}) || !reflect.DeepEqual(report.Configured, []string{"blue"}) {
		t.Fatalf("unusable %v, configured %v", report.Unusable, report.Configured)
	}
	wantNotes := []string{
		"Skipped Red: " + filepath.Join(c.home, ".red", "hooks.json") + ": invalid character '\"' after object key:value pair.",
		"Next: fix that file, then run zalki install.",
		"Not found: Green. Install one, then run zalki install.",
	}
	if !reflect.DeepEqual(report.Notes, wantNotes) {
		t.Fatalf("notes = %q\nwant %q", report.Notes, wantNotes)
	}
	if c.read(".red/hooks.json") != `{"a": 1 "b": 2}` || !c.exists(".blue/hooks.json") {
		t.Fatal("the unusable file was changed, or the other client was not configured")
	}
	if got := c.recorded(); !reflect.DeepEqual(got, []string{".blue/hooks.json"}) {
		t.Fatalf("the record lists %v, want only the file that was written", got)
	}
	// After the user repairs the file, the next upgrade configures the client.
	c.write(".red/hooks.json", `{}`)
	if report := c.run("upgrade", goldenBinary); len(report.Unusable) != 0 || !strings.Contains(c.read(".red/hooks.json"), "red") {
		t.Fatalf("the repaired file was not configured: %+v", report)
	}
}

// A skipped client keeps its entries on record, so an uninstall afterwards still removes what zalki wrote.
func TestASkippedClientKeepsItsEntriesOnRecord(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".red", ".blue")
	c.run("install", goldenBinary)
	redBefore := c.read(".red/hooks.json")
	c.write(".red/hooks.json", `{"broken": `)
	report := c.run("upgrade", goldenNewBinary)
	if !reflect.DeepEqual(report.Unusable, []string{"Red"}) || !strings.Contains(c.read(".blue/hooks.json"), goldenNewBinary) {
		t.Fatalf("unusable %v; blue was not upgraded", report.Unusable)
	}
	if !slices.Contains(report.Notes, "Next: fix that file, then run zalki upgrade.") {
		t.Fatalf("notes = %q; the hint of an upgrade names upgrade", report.Notes)
	}
	if got := c.recorded(); !reflect.DeepEqual(got, []string{".blue/hooks.json", ".red/hooks.json"}) {
		t.Fatalf("the record lists %v, want both files", got)
	}
	c.write(".red/hooks.json", redBefore)
	c.run("upgrade", goldenNewBinary)
	if !strings.Contains(c.read(".red/hooks.json"), goldenNewBinary) || strings.Contains(c.read("state/installation.json"), goldenBinary) {
		t.Fatal("after the repair the red entry did not move to the new binary, or the record still names the old one")
	}
}

// A client that is not here now keeps what zalki wrote earlier: nothing is read, written or removed.
func TestAClientThatIsNoLongerHereKeepsItsRecordAndFiles(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".red", ".blue")
	c.run("install", goldenBinary)
	blueFile := c.read(".blue/hooks.json")
	if err := os.RemoveAll(filepath.Join(c.home, ".blue")); err != nil {
		t.Fatal(err)
	}
	report := c.run("upgrade", goldenNewBinary)
	if !reflect.DeepEqual(report.Configured, []string{"red"}) || c.exists(".blue") {
		t.Fatalf("configured %v; the missing client's directory was created", report.Configured)
	}
	if got := c.recorded(); !reflect.DeepEqual(got, []string{".blue/hooks.json", ".red/hooks.json"}) {
		t.Fatalf("the record lists %v, want both files", got)
	}
	c.write(".blue/hooks.json", blueFile)
	if report := c.run("upgrade", goldenNewBinary); !reflect.DeepEqual(report.Configured, []string{"blue", "red"}) {
		t.Fatalf("configured %v after the client came back", report.Configured)
	}
}

// Uninstall removes what the record names, whatever is present, and skips an unusable file the same way.
func TestUninstallSkipsOnlyTheClientWithAnUnusableFile(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".red", ".blue")
	c.run("install", goldenBinary)
	c.write(".red/hooks.json", `{"broken": `)
	report := c.run("uninstall", goldenBinary)
	if !reflect.DeepEqual(report.Unusable, []string{"Red"}) || strings.Contains(c.read(".blue/hooks.json"), "zalki") {
		t.Fatalf("unusable %v; blue still holds the zalki entry", report.Unusable)
	}
	if want := "Next: fix that file, then run zalki uninstall."; report.Notes[len(report.Notes)-1] != want {
		t.Fatalf("notes = %q", report.Notes)
	}
	if got := c.recorded(); !reflect.DeepEqual(got, []string{".red/hooks.json"}) {
		t.Fatalf("the record lists %v, want only the red file", got)
	}
	c.write(".red/hooks.json", `{"zalki": `+jsonOf(t, clientEntry(goldenBinary, "red"))+`}`)
	c.run("uninstall", goldenBinary)
	if c.exists("state/installation.json") || strings.Contains(c.read(".red/hooks.json"), "zalki") {
		t.Fatal("the second uninstall did not finish the job")
	}
}

// Uninstall never skips a client for being absent: it removes what the record says,
// also in a place that the client's variable no longer points to.
func TestUninstallDoesNotSkipAClientThatIsNotHere(t *testing.T) {
	c := newClientHome(t)
	moved := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BLUE_HOME", moved)
	c.run("install", goldenBinary)
	t.Setenv("BLUE_HOME", "")
	report := c.run("uninstall", goldenBinary)
	raw, err := os.ReadFile(filepath.Join(moved, "hooks.json"))
	if err != nil || len(report.Unusable) != 0 || strings.Contains(string(raw), "zalki") || c.exists("state/installation.json") {
		t.Fatalf("unusable %v, file %q (%v): the entry of a client that is not found now was not removed", report.Unusable, raw, err)
	}
}

// A preview tells the same story and writes nothing.
func TestAPreviewReportsTheSkippedClientsAndWritesNothing(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".blue")
	c.write(".red/hooks.json", `{"a" 1}`)
	before := snapshotHome(t, c.home)
	report, err := install.Manage(threeClients, c.request("install", goldenBinary, false))
	if err != nil || !report.DryRun || !reflect.DeepEqual(report.Unusable, []string{"Red"}) || len(report.Paths) != 1 {
		t.Fatalf("report %+v, error %v", report, err)
	}
	if !equalFiles(before, snapshotHome(t, c.home)) {
		t.Fatal("a preview wrote files")
	}
}

func jsonUnmarshal(raw []byte, into any) error { return json.Unmarshal(raw, into) }

func sortStrings(names []string) { sort.Strings(names) }

func jsonOf(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A client's files are all or nothing: when one of them cannot be used, none of its files is written.
func TestAClientWithTwoFilesIsSkippedWhenOneFileCannotBeUsed(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".red")
	c.write(".green/hooks.json", `{"zalki": `)
	report := c.run("install", goldenBinary)
	if !reflect.DeepEqual(report.Unusable, []string{"Green"}) || !reflect.DeepEqual(report.Configured, []string{"red"}) {
		t.Fatalf("unusable %v, configured %v", report.Unusable, report.Configured)
	}
	if c.exists(".green.json") {
		t.Fatal("one file of a skipped client was written")
	}
	if got := c.recorded(); !reflect.DeepEqual(got, []string{".red/hooks.json"}) {
		t.Fatalf("the record lists %v, want only the red file", got)
	}
}

// A file with no client is always configured: nothing decides that it is absent.
func TestAFileWithNoClientIsAlwaysConfigured(t *testing.T) {
	c := newClientHome(t)
	declare := func(binary, state string) ([]install.ConfigFile, error) {
		files, err := threeClients(binary, state)
		return append(files, install.ConfigFile{Path: ".plain/hooks.json", Draft: "plain.json", JSON: map[string]any{"zalki": clientEntry(binary, "plain")}, OwnedEntry: "zalki"}), err
	}
	report, err := install.Manage(declare, c.request("install", goldenBinary, true))
	if err != nil || !c.exists(".plain/hooks.json") || c.exists(".red") {
		t.Fatalf("report %+v, error %v; want only the file with no client written", report, err)
	}
	if want := []string{"Not found: Blue, Green, Red. Install one, then run zalki install."}; !reflect.DeepEqual(report.Notes, want) {
		t.Fatalf("notes = %q, want %q", report.Notes, want)
	}
}

// A client with no command can only be present by its directory: a lookup of an empty name finds nothing.
func TestAClientWithNoCommandIsPresentOnlyByItsDirectory(t *testing.T) {
	c := newClientHome(t)
	declare := func(binary, state string) ([]install.ConfigFile, error) {
		return []install.ConfigFile{{Path: ".quiet/hooks.json", Draft: "quiet.json", JSON: map[string]any{"zalki": clientEntry(binary, "quiet")}, OwnedEntry: "zalki", Client: "quiet", ClientLabel: "Quiet"}}, nil
	}
	request := c.request("install", goldenBinary, true)
	request.FindExecutable = func(string) bool { return true }
	if _, err := install.Manage(declare, request); err == nil || !strings.HasPrefix(err.Error(), "No supported client found: Quiet.") {
		t.Fatalf("error %v: a client with no command was found by an empty lookup", err)
	}
}

// When several files of a client cannot be used, the line names the first one, in path order.
func TestTheSkippedLineNamesTheFirstUnusableFile(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".red")
	c.write(".green.json", `{"first": `)
	c.write(".green/hooks.json", `{"second": `)
	report := c.run("install", goldenBinary)
	if len(report.Notes) < 1 || !strings.HasPrefix(report.Notes[0], "Skipped Green: "+filepath.Join(c.home, ".green.json")+": ") {
		t.Fatalf("notes = %q; want the first file in path order", report.Notes)
	}
}

// The fragments of a TOML file stay on record for a client that is not here, like the patches of a JSON file.
func TestAnAbsentClientKeepsTheFragmentOfItsTOMLFileOnRecord(t *testing.T) {
	c := newClientHome(t)
	declare := func(binary, state string) ([]install.ConfigFile, error) {
		files, err := threeClients(binary, state)
		return append(files, install.ConfigFile{Path: ".amber/config.toml", Draft: "amber.toml", TOML: "\n[mcp_servers.zalki]\ncommand = \"" + binary + "\"\n", Client: "amber", ClientLabel: "Amber"}), err
	}
	c.mkdir(".red", ".amber")
	if _, err := install.Manage(declare, c.request("install", goldenBinary, true)); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(c.home, ".amber")); err != nil {
		t.Fatal(err)
	}
	if _, err := install.Manage(declare, c.request("upgrade", goldenNewBinary, true)); err != nil {
		t.Fatal(err)
	}
	var record struct {
		Fragments map[string]string `json:"fragments"`
	}
	if err := json.Unmarshal([]byte(c.read("state/installation.json")), &record); err != nil {
		t.Fatal(err)
	}
	if got := record.Fragments[".amber/config.toml"]; !strings.Contains(got, goldenBinary) || strings.Contains(got, goldenNewBinary) {
		t.Fatalf("the record holds %q; want the fragment of the first install, unchanged", got)
	}
}

// The same command, run again, adds the client that was skipped or that appeared: install accepts
// a record that lacks it, as long as every recorded file is where the record has it.
func TestInstallRunAgainAddsAClientThatWasSkippedOrAppeared(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".blue")
	c.write(".red/hooks.json", `{"a": 1 "b": 2}`)
	if report := c.run("install", goldenBinary); !reflect.DeepEqual(report.Unusable, []string{"Red"}) {
		t.Fatalf("unusable %v", report.Unusable)
	}
	c.write(".red/hooks.json", `{}`)
	c.mkdir(".green")
	report := c.run("install", goldenBinary)
	if !reflect.DeepEqual(report.Configured, []string{"blue", "green", "red"}) || len(report.Unusable) != 0 {
		t.Fatalf("configured %v, unusable %v; want all three", report.Configured, report.Unusable)
	}
	if got := c.recorded(); !reflect.DeepEqual(got, []string{".blue/hooks.json", ".green.json", ".green/hooks.json", ".red/hooks.json"}) {
		t.Fatalf("the record lists %v", got)
	}
	before := snapshotHome(t, c.home)
	c.run("install", goldenBinary)
	if !equalFiles(before, snapshotHome(t, c.home)) {
		t.Fatal("a third install changed files")
	}
}

// A recorded file that is somewhere else now is still refused: install does not move an installation.
func TestInstallStillRefusesToMoveARecordedFile(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".blue")
	c.run("install", goldenBinary)
	moved := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(moved, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BLUE_HOME", moved)
	_, err := install.Manage(threeClients, c.request("install", goldenBinary, true))
	if err == nil || !strings.Contains(err.Error(), "installed at another binary or configuration path") {
		t.Fatalf("error %v; want the refusal to move a recorded file", err)
	}
}

// An install of another binary over an installation is refused, also when it would add a client.
func TestInstallStillRefusesAnotherBinary(t *testing.T) {
	c := newClientHome(t)
	c.mkdir(".blue")
	c.run("install", goldenBinary)
	c.mkdir(".red")
	_, err := install.Manage(threeClients, c.request("install", goldenNewBinary, true))
	if err == nil || !strings.Contains(err.Error(), "installed at another binary or configuration path") {
		t.Fatalf("error %v; want the refusal", err)
	}
	if c.exists(".red/hooks.json") {
		t.Fatal("a refused install wrote a file")
	}
}
