package pluginfeed

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/statefile"
)

// The files in testdata/rc8 were written by the rc.8 release for the plugin
// "demo". An update must read each one and publish the same bytes when it
// writes the record again. If the record IO moves to another package, these
// tests must pass without any change to the expected bytes.

const rc8PluginID = "0d2e3385-7270-4b7f-bc3a-9843f8a136e7"

// rc8PluginRoot copies the rc.8 plugin files into a private state root.
func rc8PluginRoot(t *testing.T) pluginFiles {
	t.Helper()
	root := t.TempDir()
	plugins := filepath.Join(root, "plugins")
	if err := os.Mkdir(plugins, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(plugins, "demo.data"), 0700); err != nil {
		t.Fatal(err)
	}
	for from, to := range map[string]string{
		"demo.json":            "demo.json",
		"demo.feed-state.json": "demo.feed-state.json",
		"demo.inbox.json":      "demo.data/inbox.json",
	} {
		if err := os.WriteFile(filepath.Join(plugins, to), rc8Bytes(t, from), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return pluginFiles{root: root, name: "demo"}
}

func rc8Bytes(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "rc8", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func requireRC8File(t *testing.T, path, name string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := rc8Bytes(t, name); !bytes.Equal(got, want) {
		t.Fatalf("written file differs from the rc.8 bytes\ngot:  %q\nwant: %q", got, want)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("mode of %s: %v, %v; want 0600", path, info, err)
	}
}

func TestRC8PluginRecordIsReadAndWrittenWithTheSameBytes(t *testing.T) {
	files := rc8PluginRoot(t)
	record, raw, err := files.loadPlugin()
	if err != nil || record.Name != "demo" || record.ID != rc8PluginID {
		t.Fatalf("load: %+v, %v", record, err)
	}
	if !bytes.Equal(raw, rc8Bytes(t, "demo.json")) {
		t.Fatal("the bytes returned by the load differ from the rc.8 file")
	}
	if want := pluginRevision(record); record.Revision != want {
		t.Fatalf("stored revision %s, recomputed %s", record.Revision, want)
	}
	if err := os.Remove(files.pluginPath()); err != nil {
		t.Fatal(err)
	}
	written, err := files.savePlugin(record)
	if err != nil || !bytes.Equal(written, rc8Bytes(t, "demo.json")) {
		t.Fatalf("save returned %q, %v", written, err)
	}
	requireRC8File(t, files.pluginPath(), "demo.json")
}

func TestRC8FeedStateIsReadAndWrittenWithTheSameBytes(t *testing.T) {
	files := rc8PluginRoot(t)
	state, exists, err := files.loadStreamState(newStreamBudget(t.Context()))
	if err != nil || !exists {
		t.Fatalf("load: exists %v, %v", exists, err)
	}
	if err := os.Remove(files.streamPath()); err != nil {
		t.Fatal(err)
	}
	if err := files.saveStreamState(&state); err != nil {
		t.Fatal(err)
	}
	requireRC8File(t, files.streamPath(), "demo.feed-state.json")
}

func TestRC8InboxIndexIsReadAndWrittenWithTheSameBytes(t *testing.T) {
	files := rc8PluginRoot(t)
	path := filepath.Join(files.pluginDataPath(), "inbox.json")
	raw, err := statefile.ReadBytes(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var index pluginInboxIndex
	if err := statefile.DecodeStrictRecordJSON(raw, &index); err != nil {
		t.Fatalf("the strict decoder refuses the rc.8 inbox index: %v", err)
	}
	index.adopted = true // as the loader sets it for a list read from the data folder
	if err := files.savePluginInboxIndex(index); err != nil {
		t.Fatal(err)
	}
	requireRC8File(t, files.inboxIndexPath(), "demo.inbox.json")
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("the earlier file is still in the data folder after the move: %v", err)
	}
}

func TestRC8PluginIdentityFileIsWrittenWithTheSameBytes(t *testing.T) {
	files := rc8PluginRoot(t)
	if err := files.savePluginIdentity(rc8PluginID); err != nil {
		t.Fatal(err)
	}
	requireRC8File(t, filepath.Join(files.pluginDir(), "id-"+rc8PluginID+".json"), "id-"+rc8PluginID+".json")
}
