package pluginfeed

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/statefile"
)

// The inbox index is Zalki's file. It must live outside the plugin's data
// folder, move there from an earlier build with its quota, and never touch a file the
// plugin wrote.

func (x inboxStorageFixture) admit(t *testing.T) model.Message {
	t.Helper()
	m, err := x.service.Send(context.Background(), x.request())
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func readFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s exists: %v", path, err)
	}
}

// asEarlier moves the index file back to where an earlier build kept it.
func (x inboxStorageFixture) asEarlier(t *testing.T) []byte {
	t.Helper()
	raw := readFileBytes(t, x.f.inboxIndexPath())
	if err := os.Rename(x.f.inboxIndexPath(), x.f.earlierInboxIndexPath()); err != nil {
		t.Fatal(err)
	}
	return raw
}

func (x inboxStorageFixture) loaded(t *testing.T) (pluginInboxIndex, error) {
	t.Helper()
	var index pluginInboxIndex
	err := x.f.write(func(view bus.JournalView) error {
		var err error
		index, err = x.f.loadPluginInboxIndex(view, x.status.ID, time.Now().UTC())
		return err
	})
	return index, err
}

func (x inboxStorageFixture) load(t *testing.T) error {
	t.Helper()
	_, err := x.loaded(t)
	return err
}

func TestInboxIndexIsKeptOutsideThePluginDataFolder(t *testing.T) {
	x := newInboxStorageFixture(t)
	m := x.admit(t)
	info, err := os.Stat(x.f.inboxIndexPath())
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("index file: %v, %v", info, err)
	}
	if filepath.Dir(x.f.inboxIndexPath()) != x.f.pluginDir() || filepath.Base(x.f.inboxIndexPath()) != "external.inbox.json" {
		t.Fatalf("index path %s", x.f.inboxIndexPath())
	}
	got, code := x.f.NextPluginInbox(&x.status)
	if got == nil || got.ID != m.ID || code != "ok" {
		t.Fatalf("next: %+v %q", got, code)
	}
	if code := x.f.ConsumePluginInbox(&x.status, m.ID); code != "ok" {
		t.Fatalf("consume: %q", code)
	}
	entries, err := os.ReadDir(x.f.pluginDataPath())
	if err != nil || len(entries) != 0 {
		t.Fatalf("the plugin's data folder holds %d entries after a full cycle: %v", len(entries), err)
	}
}

func TestPluginFileNamedInboxJSONIsNeverReadChangedOrDeleted(t *testing.T) {
	other := model.NewUUID()
	cases := []struct {
		name    string
		content []byte
		note    bool // doctor names it: it has the keys of a Zalki list
	}{
		{"garbage", []byte("not json at all"), false},
		{"other shape", []byte(`{"entries":"mine","note":"plugin data"}`), false},
		{"another installation", []byte(`{"version":1,"installation":"` + other + `","entries":[],"quota":{}}`), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			checkPluginFileSurvives(t, tc.content, tc.note)
		})
	}
}

func checkPluginFileSurvives(t *testing.T, content []byte, note bool) {
	t.Helper()
	x := newInboxStorageFixture(t)
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	mine := x.f.earlierInboxIndexPath()
	if err := os.WriteFile(mine, content, 0600); err != nil {
		t.Fatal(err)
	}
	m := x.admit(t)
	if code := x.f.ConsumePluginInbox(&x.status, m.ID); code != "ok" {
		t.Fatalf("consume: %q", code)
	}
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	requireFileBytes(t, mine, content, "the plugin's file changed")
	if problem := x.f.InboxIndexProblem(); (problem != "") != note || (note && !strings.Contains(problem, mine)) {
		t.Fatalf("doctor sentence %q, want one: %v", problem, note)
	}
}

func TestSettleMovesAnEarlierIndexWithItsQuota(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.admit(t)
	x.admit(t)
	want := x.asEarlier(t)
	if index, err := x.loaded(t); err != nil || len(index.Entries) != 2 {
		t.Fatalf("until the move, inbox calls must still read the earlier file: %+v, %v", index, err)
	}
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	if got := readFileBytes(t, x.f.inboxIndexPath()); !bytes.Equal(got, want) {
		t.Fatalf("the moved index differs from the earlier file\ngot:  %q\nwant: %q", got, want)
	}
	requireAbsent(t, x.f.earlierInboxIndexPath())
	index := x.index(t)
	if len(index.Entries) != 2 || len(index.Quota.Recent) != 2 {
		t.Fatalf("entries %d, recent arrivals %d: the move lost entries or reset the quota", len(index.Entries), len(index.Quota.Recent))
	}
}

func TestSettleStartsAnEmptyIndexOnceAndChangesNothingAfter(t *testing.T) {
	x := newInboxStorageFixture(t)
	requireAbsent(t, x.f.inboxIndexPath())
	if index, err := x.loaded(t); err != nil || index.Entries == nil || len(index.Entries) != 0 {
		t.Fatalf("an absent list must load as an empty list that can be written back: %+v, %v", index, err)
	}
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	first := readFileBytes(t, x.f.inboxIndexPath())
	if index := x.index(t); index.Version != 1 || index.Installation != x.status.ID || len(index.Entries) != 0 {
		t.Fatalf("empty index: %+v", index)
	}
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, readFileBytes(t, x.f.inboxIndexPath())) {
		t.Fatal("a second settle changed the index")
	}
}

func TestWhenBothFilesExistTheFileInPluginsWinsAndAValidOldOneGoes(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.admit(t)
	current := readFileBytes(t, x.f.inboxIndexPath())
	old, err := statefile.EncodeJSON(pluginInboxIndex{Version: 1, Installation: x.status.ID, Entries: []pluginInboxEntry{}, Quota: model.NewFloodState(time.Now().UTC())}, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteAtomic(x.f.earlierInboxIndexPath(), old); err != nil {
		t.Fatal(err)
	}
	if index, err := x.loaded(t); err != nil || len(index.Entries) != 1 {
		t.Fatalf("with both files present the load must use the file in plugins/: %+v, %v", index, err)
	}
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, readFileBytes(t, x.f.inboxIndexPath())) {
		t.Fatal("the file in plugins/ changed")
	}
	requireAbsent(t, x.f.earlierInboxIndexPath())
}

// A file named inbox.json in the data folder that is not a list of ours is left
// untouched and the plugin starts. A plugin's own data produces no doctor line;
// a damaged Zalki list (it has the keys of our list) is named by the doctor and
// logged as a warning. Neither log line has a name or a path.
func TestAFileThatIsNotOurListIsLeftAsPluginData(t *testing.T) {
	for name, tc := range notOurListCases() {
		t.Run(name, func(t *testing.T) {
			checkFileLeftInPlace(t, tc)
		})
	}
}

type notOurList struct {
	content func(t *testing.T, x inboxStorageFixture) []byte
	damaged bool // it has the keys of a Zalki list
}

func notOurListCases() map[string]notOurList {
	return map[string]notOurList{
		"garbage":     {func(*testing.T, inboxStorageFixture) []byte { return []byte("not json at all") }, false},
		"other shape": {func(*testing.T, inboxStorageFixture) []byte { return []byte(`{"entries":"mine","note":"plugin data"}`) }, false},
		"another installation": {func(*testing.T, inboxStorageFixture) []byte {
			return []byte(`{"version":1,"installation":"` + model.NewUUID() + `","entries":[],"quota":{}}`)
		}, true},
		"our shape with a repeated message": {func(t *testing.T, x inboxStorageFixture) []byte {
			t.Helper()
			index := x.index(t)
			index.Entries = append(index.Entries, index.Entries[0])
			raw, err := statefile.EncodeJSON(index, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			return raw
		}, true},
	}
}

func checkFileLeftInPlace(t *testing.T, tc notOurList) {
	t.Helper()
	x := newInboxStorageFixture(t)
	x.admit(t)
	raw := tc.content(t, x)
	if err := os.Remove(x.f.inboxIndexPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(x.f.earlierInboxIndexPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	x.f.store = x.f.store.With(bus.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatalf("the plugin must start: %v", err)
	}
	requireFileBytes(t, x.f.earlierInboxIndexPath(), raw, "the plugin's file was changed")
	if index := x.index(t); index.Version != 1 || index.Installation != x.status.ID || len(index.Entries) != 0 {
		t.Fatalf("the new list must be empty: %+v", index)
	}
	requireDoctorSentence(t, x, tc.damaged)
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	requireOneEventWithoutNames(t, logs.String(), x.f.name, tc.damaged)
	if m := x.admit(t); m.ID == "" {
		t.Fatal("a send was refused")
	}
	requireFileBytes(t, x.f.earlierInboxIndexPath(), raw, "the plugin's file changed after an admission")
}

func requireDoctorSentence(t *testing.T, x inboxStorageFixture, damaged bool) {
	t.Helper()
	problem := x.f.InboxIndexProblem()
	if !damaged {
		if problem != "" {
			t.Fatalf("a plugin's own file must not be a doctor line: %q", problem)
		}
		return
	}
	if !strings.Contains(problem, x.f.earlierInboxIndexPath()) || !strings.Contains(problem, "looks like a Zalki inbox list but cannot be used") {
		t.Fatalf("doctor sentence: %q", problem)
	}
}

func requireFileBytes(t *testing.T, path string, want []byte, message string) {
	t.Helper()
	if got := readFileBytes(t, path); !bytes.Equal(got, want) {
		t.Fatalf("%s: %q", message, got)
	}
}

func requireOneEventWithoutNames(t *testing.T, logs, pluginName string, damaged bool) {
	t.Helper()
	level, reason := "INFO", "plugin_file"
	if damaged {
		level, reason = "WARN", "damaged_zalki_list"
	}
	lines := strings.Split(strings.TrimSpace(logs), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"msg":"plugin.inbox.file_left"`) || !strings.Contains(lines[0], `"level":"`+level+`"`) ||
		!strings.Contains(lines[0], `"reason":"`+reason+`"`) || strings.Contains(lines[0], "inbox.json") || strings.Contains(lines[0], pluginName) {
		t.Fatalf("want one %s event %q without a name or a path, got %q", level, reason, logs)
	}
}

func TestAnUnreadableIndexInPluginsFailsClosedAndNamesTheFile(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.admit(t)
	if err := os.WriteFile(x.f.inboxIndexPath(), []byte("damaged"), 0600); err != nil {
		t.Fatal(err)
	}
	problem := x.f.InboxIndexProblem()
	if !strings.Contains(problem, x.f.inboxIndexPath()) || strings.Contains(problem, "data folder") || !strings.Contains(problem, "It is Zalki's file") {
		t.Fatalf("the file in plugins/ is Zalki's, not the plugin's: %q", problem)
	}
	if err := x.load(t); err == nil || !strings.Contains(err.Error(), x.f.inboxIndexPath()) || !errors.Is(err, statefile.ErrInvalidFile) {
		t.Fatalf("an inbox call must fail closed and name the file: %v", err)
	}
	r := x.request()
	if m, err := x.service.Send(context.Background(), r); err == nil || m.ID != "" {
		t.Fatalf("a send was accepted: %+v, %v", m, err)
	}
	x.assertAbsent(t, r.MessageID)
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	if string(readFileBytes(t, x.f.inboxIndexPath())) != "damaged" {
		t.Fatal("settle rewrote an unreadable index")
	}
}

// The earlier copy may be the only readable list: it is kept while the file in
// plugins/ cannot be read.
func TestAnUnreadableIndexInPluginsKeepsAValidEarlierCopy(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.admit(t)
	copyOfList := readFileBytes(t, x.f.inboxIndexPath())
	if err := fsutil.WriteAtomic(x.f.earlierInboxIndexPath(), copyOfList); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(x.f.inboxIndexPath(), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	if got := readFileBytes(t, x.f.earlierInboxIndexPath()); !bytes.Equal(got, copyOfList) {
		t.Fatal("the earlier copy was removed or changed while the file in plugins/ cannot be read")
	}
	if string(readFileBytes(t, x.f.inboxIndexPath())) != "{not json" {
		t.Fatal("settle changed the unreadable file")
	}
}

// Once the file in plugins/ exists, a copy that looks like a valid list in the
// data folder is not read.
func TestAnEarlierCopyPlantedWhileTheNewFileExistsIsIgnored(t *testing.T) {
	x := newInboxStorageFixture(t)
	m := x.admit(t)
	copyOfList := readFileBytes(t, x.f.inboxIndexPath())
	if code := x.f.ConsumePluginInbox(&x.status, m.ID); code != "ok" {
		t.Fatalf("consume: %q", code)
	}
	if err := fsutil.WriteAtomic(x.f.earlierInboxIndexPath(), copyOfList); err != nil {
		t.Fatal(err)
	}
	if got, code := x.f.NextPluginInbox(&x.status); got != nil || code != "ok" {
		t.Fatalf("an inbox call offered %+v (%q) from a copy in the data folder", got, code)
	}
	if index, err := x.loaded(t); err != nil || len(index.Entries) != 0 {
		t.Fatalf("the load read the data folder: %+v, %v", index, err)
	}
}

func TestRemovingAPluginSweepsItsIndexAndRefusesAForeignOne(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.admit(t)
	if err := os.WriteFile(x.f.inboxIndexPath(), []byte("not ours"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := x.f.RemovePlugin(); err == nil || !strings.Contains(err.Error(), x.f.inboxIndexPath()) {
		t.Fatalf("remove with a foreign index file: %v", err)
	}
	if string(readFileBytes(t, x.f.inboxIndexPath())) != "not ours" {
		t.Fatal("a foreign file was changed or removed")
	}
	y := newInboxStorageFixture(t)
	y.admit(t)
	if err := y.f.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	requireAbsent(t, y.f.inboxIndexPath())
	requireAbsent(t, y.f.pluginDataPath())
}

// A send can arrive after an update and before the plugin starts again. The
// list an earlier build kept must not be lost then.
func TestAnEarlierListIsNotLostWhenASendComesBeforeTheFirstStart(t *testing.T) {
	x := newInboxStorageFixture(t)
	first, second := x.admit(t), x.admit(t)
	x.asEarlier(t)
	third := x.admit(t) // before any settle
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, entry := range x.index(t).Entries {
		got[entry.ID] = true
	}
	for _, m := range []model.Message{first, second, third} {
		if !got[m.ID] {
			t.Fatalf("message %s is no longer in the inbox list: %v", m.ID, got)
		}
	}
}

// An entry that is not valid makes the whole list unreadable: the error names
// the file in this case too.
func TestAListWithAnInvalidEntryNamesTheFile(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.admit(t)
	index := x.index(t)
	index.Entries = append(index.Entries, index.Entries[0]) // the same message twice
	x.putIndex(t, index)
	if err := x.load(t); err == nil || !strings.Contains(err.Error(), x.f.inboxIndexPath()) || !errors.Is(err, statefile.ErrInvalidFile) {
		t.Fatalf("the error must name the file and stay an invalid-file error: %v", err)
	}
}

// A list with the shape of ours that the journal does not accept (here an entry
// whose sender kind this build does not know) is never adopted and never
// deleted: it is a damaged list, left in place and named.
func TestAnEarlierListThatTheJournalRejectsIsKeptAsADamagedList(t *testing.T) {
	x := newInboxStorageFixture(t)
	x.admit(t)
	index := x.index(t)
	index.Entries[0].Sender = "ghostkind:" + model.NewUUID()
	raw, err := statefile.EncodeJSON(index, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(x.f.inboxIndexPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(x.f.earlierInboxIndexPath(), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	x.f.store = x.f.store.With(bus.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	if err := x.f.SettleInboxIndex(); err != nil {
		t.Fatalf("the plugin must start: %v", err)
	}
	requireFileBytes(t, x.f.earlierInboxIndexPath(), raw, "the list was removed or changed")
	if index := x.index(t); len(index.Entries) != 0 {
		t.Fatalf("the new list must be empty: %+v", index)
	}
	requireDoctorSentence(t, x, true)
	requireOneEventWithoutNames(t, logs.String(), x.f.name, true)
	x.admit(t)
	requireFileBytes(t, x.f.earlierInboxIndexPath(), raw, "the list was removed after an admission")
}
