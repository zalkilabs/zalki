package pluginfeed

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// commitWith installs "demo" with the given access, declared access and
// installer, and returns the result.
func commitWith(t *testing.T, s bus.Store, access PluginAccess, declared, actor, key string, review bool) (PluginFeed, error) {
	t.Helper()
	f := NewPluginFeed(s, "demo")
	install, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = install.Close() }()
	receipt := testInstallReceipt()
	receipt.Access = json.RawMessage(declared)
	_, err = install.Commit(stagedTestArtifact(t, install), nil, access, receipt, actor, key, review)
	return f, err
}

const declaredAll = `{"status":true,"receive":true,"send":true,"read":true}`

// U30: the receipt keeps exactly the four declared booleans, in canonical form.
func TestPluginInstallReceiptAccess(t *testing.T) {
	for raw, ok := range map[string]bool{
		declaredAll: true,
		`{"status":true,"receive":false,"send":false,"read":false}`:               true,
		`{"status":true,"receive":false,"send":false}`:                            false,
		`{"status":true,"receive":false,"send":false,"read":false,"spawn":true}`:  false,
		`{"status":true,"receive":false,"send":false,"read":null}`:                false,
		`{"status":"true","receive":false,"send":false,"read":false}`:             false,
		`{"receive":false,"status":true,"send":false,"read":false}`:               false,
		`{"status": true,"receive":false,"send":false,"read":false}`:              true,
		`{"status":true,"status":true,"receive":false,"send":false,"read":false}`: false,
		``: false, `null`: false,
	} {
		if _, got := decodeDeclaredAccess(json.RawMessage(raw)); got != ok {
			t.Errorf("%s: accepted = %v", raw, got)
		}
	}
	s := fixtureStore(t)
	f, err := commitWith(t, s, PluginAccess{Status: true}, `{"status":true,"receive":false,"send":false,"read":false}`, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if d, ok := decodeDeclaredAccess(status.Install.Access); err != nil || !ok || d != (declaredAccess{Status: true}) {
		t.Fatal("declared access not kept exactly", status.Install, err)
	}
}

// Granted access must be declared; send and read need a reviewed terminal;
// no access at all is allowed only for an extended-only declaration.
func TestCommitAccessRules(t *testing.T) {
	member := "codex:" + model.NewUUID()
	for name, test := range map[string]struct {
		access   PluginAccess
		declared string
		review   bool
		ok       bool
	}{
		"standard":                {PluginAccess{Status: true, Receive: true}, declaredAll, false, false},
		"standard declared":       {PluginAccess{Status: true}, `{"status":true,"receive":false,"send":false,"read":false}`, false, true},
		"undeclared receive":      {PluginAccess{Receive: true}, `{"status":true,"receive":false,"send":false,"read":false}`, true, false},
		"undeclared read target":  {PluginAccess{Read: []string{member}}, `{"status":true,"receive":false,"send":false,"read":false}`, true, false},
		"extended without review": {PluginAccess{}, `{"status":false,"receive":false,"send":true,"read":false}`, false, false},
		"extended, none chosen":   {PluginAccess{}, `{"status":false,"receive":false,"send":true,"read":false}`, true, true},
		"standard, none chosen":   {PluginAccess{}, `{"status":true,"receive":false,"send":false,"read":false}`, true, false},
	} {
		if err := commitAccessError(test.access, json.RawMessage(test.declared), test.review); (err == nil) != test.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// U26/U28: the installer is proven at commit. A valid session is recorded by
// its ID, an anonymous reviewed terminal as terminal, and anything else as
// unproven; the capability is never stored.
func TestPluginInstallAuthenticatedInstaller(t *testing.T) {
	_, key := register(t, fixtureStore(t), "codex")
	for name, test := range map[string]struct {
		actor, key string
		review     bool
		want       string
	}{
		"session":              {"own", "own", false, "own"},
		"session, reviewed":    {"own", "own", true, "own"},
		"anonymous, reviewed":  {"", "", true, installedByTerminal},
		"anonymous, --yes":     {"", "", false, installedByUnproven},
		"wrong key, reviewed":  {"own", "wrong", true, installedByUnproven},
		"actor only, reviewed": {"own", "", true, installedByUnproven},
		"plugin address":       {"plugin:" + model.NewUUID(), "x", true, installedByUnproven},
	} {
		t.Run(name, func(t *testing.T) { checkInstaller(t, test.actor, test.key, key, test.want, test.review) })
	}
}

// checkInstaller installs with the given proof ("own" means the new
// session's actor or key, "wrong" another store's real key) and checks the
// recorded installer and that the capability is not stored.
func checkInstaller(t *testing.T, actor, capability, otherKey, want string, review bool) {
	t.Helper()
	s := fixtureStore(t)
	v, k := register(t, s, "codex")
	if actor == "own" {
		actor = v.ID
	}
	switch capability {
	case "own":
		capability = k
	case "wrong":
		capability = otherKey
	}
	f, err := commitWith(t, s, PluginAccess{Status: true}, `{"status":true,"receive":false,"send":false,"read":false}`, actor, capability, review)
	if err != nil {
		t.Fatal("a standard install must succeed whatever the proof", err)
	}
	if want == "own" {
		want = v.ID
	}
	status, err := f.PluginStatus()
	if err != nil || status.Install.InstalledBy != want {
		t.Fatalf("installed_by = %+v, want %s (%v)", status.Install, want, err)
	}
	raw, err := os.ReadFile(f.pluginPath())
	if err != nil || strings.Contains(string(raw), k) {
		t.Fatal("the record stores the capability", err)
	}
	// The test sessions have no name: a proven session's label is its ID,
	// never anything taken from the capability.
	if want == v.ID && status.Install.InstalledByLabel != v.ID {
		t.Fatalf("the record stores the capability or a part of it as the label: %q", status.Install.InstalledByLabel)
	}
}

// U27: credentials that stopped proving by commit time (the session
// registered again, so its key rotated) give unproven, and the install
// still succeeds.
func TestPluginInstallInstallerRotatesDuringBuild(t *testing.T) {
	s := fixtureStore(t)
	v, oldKey := register(t, s, "codex")
	if _, _, err := s.Register("codex", strings.TrimPrefix(v.ID, "codex:"), "/workspace", nil); err != nil {
		t.Fatal(err)
	}
	f, err := commitWith(t, s, PluginAccess{Status: true}, `{"status":true,"receive":false,"send":false,"read":false}`, v.ID, oldKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if status, err := f.PluginStatus(); err != nil || status.Install.InstalledBy != installedByUnproven || status.Install.InstalledByLabel != "" {
		t.Fatal("a stale proof was kept", status.Install, err)
	}
}

// U29: the label is printable ASCII of at most 64 bytes, else the actor ID.
func TestPluginInstallHistoricalLabel(t *testing.T) {
	for in, want := range map[string]string{
		"worker-3": "worker-3", "": "", strings.Repeat("a", 64): strings.Repeat("a", 64), strings.Repeat("a", 65): "",
		"evil\x1b[2J": "", "tab\there": "", "naïve": "", "line\nbreak": "",
	} {
		if got := safeLabel(in); got != want {
			t.Errorf("safeLabel(%q) = %q", in, got)
		}
	}
	for _, by := range []string{installedByTerminal, installedByUnproven} {
		if !validProvenance(by, "") || validProvenance(by, "x") {
			t.Errorf("%s: label rule broken", by)
		}
	}
	actor := "codex:" + model.NewUUID()
	if !validProvenance(actor, actor) || validProvenance(actor, "") || validProvenance(actor, "evil\x1b") || validProvenance("plugin:"+model.NewUUID(), "x") {
		t.Error("session provenance rule broken")
	}
}

// U16/U17: a selected send/read session must be live at commit; the commit
// checks the fixed IDs and publishes nothing when one has gone.
func TestPluginInstallTargetExitBeforeCommit(t *testing.T) {
	s := fixtureStore(t)
	target, _ := register(t, s, "codex")
	if err := s.Update(target.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	f, err := commitWith(t, s, PluginAccess{Read: []string{target.ID}}, `{"status":false,"receive":false,"send":false,"read":true}`, "", "", true)
	if model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("a retired target was committed", err)
	}
	assertNoInstall(t, f)
}

// An extended-only install with no chosen session is installed, enabled and
// granted nothing.
func TestExtendedOnlyInstallWithNoTargets(t *testing.T) {
	s := fixtureStore(t)
	f, err := commitWith(t, s, PluginAccess{}, `{"status":false,"receive":false,"send":true,"read":true}`, "", "", true)
	if err != nil {
		t.Fatal(err)
	}
	status, err := f.PluginStatus()
	if err != nil || !status.Enabled || !status.Access.empty() {
		t.Fatal("an extended-only install with no targets did not load enabled and empty", status, err)
	}
}

// U44: a receipt from before install provenance is an old record: never
// completed with a guessed installer.
func TestPluginOldRecordRefusal(t *testing.T) {
	s := fixtureStore(t)
	f := installForTest(t, s)
	raw, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"installed_by", "access"} {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		delete(record["install"].(map[string]any), key)
		old, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.pluginPath(), old, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.PluginStatus(); !errors.Is(err, ErrOldPluginRecord) {
			t.Fatalf("a receipt without %s is not an old record: %v", key, err)
		}
	}
}

// startNoticeFixture installs "demo" and returns it with its record.
func startNoticeFixture(t *testing.T) (PluginFeed, pluginRecord) {
	t.Helper()
	f := installForTest(t, fixtureStore(t))
	p, _, err := f.loadPlugin()
	if err != nil {
		t.Fatal(err)
	}
	return f, p
}

// U31: notices are due only from installed_at to 24 hours after; a future or
// invalid time is never due.
func TestPluginNoticeTimeWindow(t *testing.T) {
	_, p := startNoticeFixture(t)
	at, err := time.Parse(time.RFC3339Nano, p.Install.InstalledAt)
	if err != nil {
		t.Fatal(err)
	}
	for offset, want := range map[time.Duration]bool{0: true, time.Hour: true, 24*time.Hour - time.Nanosecond: true, 24 * time.Hour: false, -time.Second: false} {
		if got := startNoticeDue(p, at.Add(offset)); got != want {
			t.Errorf("at +%v: due = %v", offset, got)
		}
	}
	p.Install = nil
	if startNoticeDue(p, at) {
		t.Error("a record without a receipt is due")
	}
}

// U32/U33: at most three starts per installation; a repeated start (same
// actor and generation) never spends twice.
func TestPluginNoticeGlobalThree(t *testing.T) {
	f, p := startNoticeFixture(t)
	now := time.Now().UTC()
	actor := "codex:" + model.NewUUID()
	first := model.NewUUID()
	reserve := func(generation string) bool {
		t.Helper()
		ok, err := f.reserveStartNotice(p, actor, generation, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	reserved, again := reserve(first), reserve(first)
	if !reserved || again {
		t.Fatal("the first start was not reserved once")
	}
	for _, start := range []string{"second", "third"} {
		if !reserve(model.NewUUID()) {
			t.Fatal("the", start, "start was refused")
		}
	}
	if reserve(model.NewUUID()) {
		t.Fatal("a fourth start was reserved")
	}
}

// U38/U39: a corrupt ledger or one of another installation is an error and
// is never reset or adopted.
func TestPluginNoticeCorruptLedgerAndInstallationFence(t *testing.T) {
	f, p := startNoticeFixture(t)
	now := time.Now().UTC()
	for name, content := range map[string]string{
		"corrupt":               `{"installation":`,
		"other installation":    `{"installation":"` + model.NewUUID() + `","starts":[]}`,
		"duplicate token":       `{"installation":"` + p.ID + `","starts":[{"actor":"codex:00000000-0000-4000-8000-000000000001","generation":"00000000-0000-4000-8000-000000000002"},{"actor":"codex:00000000-0000-4000-8000-000000000001","generation":"00000000-0000-4000-8000-000000000002"}]}`,
		"unknown field":         `{"installation":"` + p.ID + `","starts":[],"x":1}`,
		"plugin actor in token": `{"installation":"` + p.ID + `","starts":[{"actor":"plugin:00000000-0000-4000-8000-000000000001","generation":"00000000-0000-4000-8000-000000000002"}]}`,
	} {
		if err := os.WriteFile(f.startNoticePath(), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		if ok, err := f.reserveStartNotice(p, "codex:"+model.NewUUID(), model.NewUUID(), now); ok || err == nil {
			t.Errorf("%s: reserved %v, err %v", name, ok, err)
		}
		if after, _ := os.ReadFile(f.startNoticePath()); string(after) != content {
			t.Errorf("%s: the ledger was rewritten", name)
		}
	}
	// Remove refuses a ledger it cannot prove is Zalki's, and keeps it.
	corrupt, _ := os.ReadFile(f.startNoticePath())
	if err := f.RemovePlugin(); err == nil || !strings.Contains(err.Error(), "is not a file Zalki made for this plugin") {
		t.Fatal("remove deleted or ignored a corrupt ledger", err)
	}
	if after, err := os.ReadFile(f.startNoticePath()); err != nil || !bytes.Equal(after, corrupt) {
		t.Fatal("a refused remove changed the corrupt ledger", err)
	}
	// After the owner removes that file, a real ledger goes with the plugin.
	if err := os.Remove(f.startNoticePath()); err != nil {
		t.Fatal(err)
	}
	if ok, err := f.reserveStartNotice(p, "codex:"+model.NewUUID(), model.NewUUID(), now); !ok || err != nil {
		t.Fatal("a fresh ledger was not written", ok, err)
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.startNoticePath()); !os.IsNotExist(err) {
		t.Fatal("remove kept the start-notice ledger", err)
	}
}

// Before the copied manifest and the choices are checked, an install touches
// only its own staging: an earlier attempt's pin, reply ledger and
// start-notice ledger stay byte for byte. Only the checked Commit sweeps them,
// and only because they prove to be Zalki's own orphans.
func TestInstallSweepsArtifactsOnlyAtCheckedCommit(t *testing.T) {
	s := fixtureStore(t)
	f := NewPluginFeed(s, "demo")
	sentinels := plantOrphanArtifacts(t, f)
	install, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	receipt := testInstallReceipt() // declares send/read: refused without review
	if _, err := install.Commit(stagedTestArtifact(t, install), nil, PluginAccess{Status: true}, receipt, "", "", false); err == nil {
		t.Fatal("send/read without review was committed")
	}
	if err := install.Close(); err != nil {
		t.Fatal(err)
	}
	assertSentinels(t, sentinels)
	install, err = f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = install.Close() }()
	if _, err := install.Commit(stagedTestArtifact(t, install), nil, PluginAccess{Status: true}, standardInstallReceipt(), "", "", true); err != nil {
		t.Fatal(err)
	}
	for path := range sentinels {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("the checked commit kept the orphan %s: %v", path, err)
		}
	}
}

// plantOrphanArtifacts leaves what a crashed earlier installation of this
// name could leave: a real pin and both ledgers of a removed installation.
func plantOrphanArtifacts(t *testing.T, f PluginFeed) map[string]string {
	t.Helper()
	first, err := f.BeginPluginInstall() // creates the plugin folder
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.pluginBinPath(), 0700); err != nil {
		t.Fatal(err)
	}
	pin := "old pinned bytes"
	sum := sha256.Sum256([]byte(pin))
	old := model.NewUUID()
	sentinels := map[string]string{
		filepath.Join(f.pluginBinPath(), hex.EncodeToString(sum[:])): pin,
		f.replyLedgerPath(): `{"version":1,"installation":"` + old + `","parents":{}}`,
		f.startNoticePath(): `{"installation":"` + old + `","starts":[]}`,
	}
	for path, content := range sentinels {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return sentinels
}

// Only artifacts Zalki proves it made are removed. A link, an unknown file in
// the pin folder, a pin whose bytes do not match its name, or a ledger of
// another live installation refuses the commit with the path named, removes
// nothing, and leaves no partial sweep (U75: the defer removes nothing).
func TestInstallKeepsForeignArtifacts(t *testing.T) {
	for name, plant := range map[string]func(*testing.T, PluginFeed) string{
		"unknown file in pin folder":    plantUnknownPinFile,
		"pin with wrong bytes":          plantWrongPin,
		"pin folder is a link":          plantLinkedPinFolder,
		"ledger of a live installation": plantLiveLedger,
		"ledger that is not a ledger":   plantNonLedger,
		"skeleton reply ledger":         plantLedgerText(func(f PluginFeed) string { return f.replyLedgerPath() }, `{"installation":"%s"}`),
		"skeleton notice ledger":        plantLedgerText(func(f PluginFeed) string { return f.startNoticePath() }, `{"installation":"%s"}`),
		"null reply ledger":             plantLedgerText(func(f PluginFeed) string { return f.replyLedgerPath() }, `null`),
		"reply ledger of version 2":     plantLedgerText(func(f PluginFeed) string { return f.replyLedgerPath() }, `{"version":2,"installation":"%s","parents":{}}`),
		"notice ledger with 4 starts":   plantFourStarts,
	} {
		t.Run(name, func(t *testing.T) { checkForeignArtifactKept(t, plant, name == "pin folder is a link") })
	}
}

func plantUnknownPinFile(t *testing.T, f PluginFeed) string {
	t.Helper()
	return writeFile(t, filepath.Join(f.pluginBinPath(), "notes.txt"), "mine")
}

func plantWrongPin(t *testing.T, f PluginFeed) string {
	t.Helper()
	return writeFile(t, filepath.Join(f.pluginBinPath(), strings.Repeat("a", 64)), "not that hash")
}

func plantLinkedPinFolder(t *testing.T, f PluginFeed) string {
	t.Helper()
	if err := os.RemoveAll(f.pluginBinPath()); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, f.pluginBinPath()); err != nil {
		t.Fatal(err)
	}
	return writeFile(t, filepath.Join(target, "keep"), "outside")
}

func plantLiveLedger(t *testing.T, f PluginFeed) string {
	t.Helper()
	live := model.NewUUID()
	writeFile(t, filepath.Join(f.pluginDir(), "id-"+live+".json"), "{}")
	return writeFile(t, f.replyLedgerPath(), `{"version":1,"installation":"`+live+`","parents":{}}`)
}

// plantLedgerText writes a ledger file the host could never have written;
// %s becomes an orphan installation ID.
func plantLedgerText(path func(PluginFeed) string, text string) func(*testing.T, PluginFeed) string {
	return func(t *testing.T, f PluginFeed) string {
		t.Helper()
		if strings.Contains(text, "%s") {
			text = fmt.Sprintf(text, model.NewUUID())
		}
		return writeFile(t, path(f), text)
	}
}

func plantFourStarts(t *testing.T, f PluginFeed) string {
	t.Helper()
	starts := make([]string, 0, 4)
	for range 4 {
		starts = append(starts, `{"actor":"codex:`+model.NewUUID()+`","generation":"`+model.NewUUID()+`"}`)
	}
	return writeFile(t, f.startNoticePath(), `{"installation":"`+model.NewUUID()+`","starts":[`+strings.Join(starts, ",")+`]}`)
}

func plantNonLedger(t *testing.T, f PluginFeed) string {
	t.Helper()
	return writeFile(t, f.startNoticePath(), "my notes")
}

// fileIdentity is a file's bytes and inode, to prove it was not replaced.
type fileIdentity struct {
	bytes string
	inode uint64
}

func identityOf(t *testing.T, path string) fileIdentity {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("an unchecked install touched %s: %v", path, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("an unchecked install touched %s: %v", path, err)
	}
	return fileIdentity{string(raw), info.Sys().(*syscall.Stat_t).Ino}
}

// checkForeignArtifactKept plants valid orphans, then one foreign artifact,
// and records the bytes and inode of every planted file AFTER that. Only the
// linked-folder fixture skips the orphan pin it replaced on purpose; any
// other missing file fails. The commit must refuse by name and leave every
// recorded file present, same bytes, same inode: no sweep as you go.
func checkForeignArtifactKept(t *testing.T, plant func(*testing.T, PluginFeed) string, replacesPin bool) {
	t.Helper()
	s := fixtureStore(t)
	f := NewPluginFeed(s, "demo")
	orphans := plantOrphanArtifacts(t, f)
	kept := plant(t, f)
	snapshot := map[string]fileIdentity{kept: identityOf(t, kept)}
	for path := range orphans {
		if replacesPin && strings.HasPrefix(path, f.pluginBinPath()+string(os.PathSeparator)) {
			continue
		}
		if path != kept {
			snapshot[path] = identityOf(t, path)
		}
	}
	install, err := f.BeginPluginInstall()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = install.Close() }()
	_, err = install.Commit(stagedTestArtifact(t, install), nil, PluginAccess{Status: true}, standardInstallReceipt(), "", "", true)
	if err == nil || !strings.Contains(err.Error(), "is not a file Zalki made for this plugin") {
		t.Fatal("a foreign artifact did not refuse the commit by name", err)
	}
	for path, before := range snapshot {
		if after := identityOf(t, path); after != before {
			t.Fatalf("an unchecked install touched %s", path)
		}
	}
	if status, err := f.PluginStatus(); err != nil || status.ID != "" {
		t.Fatal("a refused commit published an installation", status, err)
	}
}

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// assertSentinels checks that every planted file still has its bytes.
func assertSentinels(t *testing.T, sentinels map[string]string) {
	t.Helper()
	for path, content := range sentinels {
		if got, err := os.ReadFile(path); err != nil || string(got) != content {
			t.Fatalf("an unchecked install touched %s: %q %v", path, got, err)
		}
	}
}
