package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// Doctor names a damaged Zalki list left in a plugin's data folder (a file
// named inbox.json with the keys of our list that cannot be used) and says
// nothing about a plugin's own data under the same name.
func TestDoctorNamesADamagedListButNotAPluginsOwnFile(t *testing.T) {
	for name, tc := range map[string]struct {
		content string
		note    bool
	}{
		"the plugin's own data": {`{"my":"own file"}`, false},
		"a damaged list":        {`{"version":1,"installation":"` + model.NewUUID() + `","entries":[],"quota":{}}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			checkDoctorInboxNote(t, tc.content, tc.note)
		})
	}
}

func checkDoctorInboxNote(t *testing.T, content string, note bool) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	feed := pluginfeed.NewPluginFeed(store, "external")
	if err := feed.AddExternalPlugin(binary, nil); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(store.Dir(), "plugins", "external.data", "inbox.json")
	if err := os.WriteFile(file, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	want := func(notes []string) bool {
		if !note {
			return len(notes) == 0
		}
		return len(notes) == 1 && strings.Contains(notes[0], file) && strings.Contains(notes[0], "plugin external") && strings.Contains(notes[0], "looks like a Zalki inbox list")
	}
	if notes := doctorBackground(cmd, &commandOptions{}, store).Notes; !want(notes) {
		t.Fatalf("doctor notes: %q", notes)
	}
	if err := feed.SettleInboxIndex(); err != nil { // the plugin starts
		t.Fatal(err)
	}
	if notes := inboxIndexNotes(store); !want(notes) {
		t.Fatalf("notes after the start: %q", notes)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if notes := inboxIndexNotes(store); len(notes) != 0 {
		t.Fatalf("notes after the file is gone: %q", notes)
	}
}

// The list file sits next to the plugin records and must never be taken for one.
func TestPluginListsIgnoreTheInboxListFile(t *testing.T) {
	for _, name := range []string{"external.inbox.json", "external.replies.json", "external.feed-state.json"} {
		if pluginRecordName.MatchString(name) {
			t.Fatalf("%s matches the plugin record name", name)
		}
		if _, ok := strings.CutSuffix(name, ".json"); ok && model.ValidKind(strings.TrimSuffix(name, ".json")) {
			t.Fatalf("%s is a valid plugin name for the supervisor scan", name)
		}
	}
}
