package pluginfeed

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// A plugin record that fails the record checks is refused with one plain error
// that names the fix, enabling it fails, and remove still works on it. The
// three shapes are the ones the removed old-record classifier used to name.
func TestUnreadablePluginRecordsGetOnePlainErrorAndCanBeRemoved(t *testing.T) {
	s := fixtureStore(t)
	f := installForTest(t, s)
	original, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	pinned := "plugin record " + f.name + " cannot be read; remove it with: zalki plugins remove " + f.name
	for name, shape := range map[string]func(m map[string]any){
		"no scope, old grant keys": func(m map[string]any) {
			delete(m, "scope")
			access := m["access"].(map[string]any)
			access["inbox_from"], access["inbox_read"] = []any{"x"}, []any{"y"}
		},
		"receipt without manifest id": func(m map[string]any) { delete(m["install"].(map[string]any), "id") },
		"receipt without installer":   func(m map[string]any) { delete(m["install"].(map[string]any), "installed_by") },
	} {
		var record map[string]any
		if err := json.Unmarshal(original, &record); err != nil {
			t.Fatal(err)
		}
		shape(record)
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.pluginPath(), raw, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.PluginStatus(); err == nil || err.Error() != pinned {
			t.Fatalf("%s: status error = %v, want %q", name, err, pinned)
		}
		if err := f.EnablePluginFeed(true); err == nil {
			t.Fatalf("%s: enabling an unreadable record succeeded", name)
		}
	}
	if err := f.RemovePlugin(); err != nil {
		t.Fatalf("remove of an unreadable record failed: %v", err)
	}
	if status, err := f.PluginStatus(); err != nil || status.ID != "" {
		t.Fatalf("after remove: id %q, error %v; want the plugin absent", status.ID, err)
	}
	if _, err := os.Lstat(f.pluginPath()); !os.IsNotExist(err) {
		t.Fatal("remove left the record file")
	}
}

// Every content check on the read path gives the same plain error, with its own
// fixed reason name for the command log, and remove works on the record and
// leaves the name free for a new install.
func TestEveryRecordContentCheckGivesThePlainErrorAndRemoveWorks(t *testing.T) {
	for _, tc := range []struct{ reason, field, value string }{
		{reasonDecode, `"scope": "all"`, `"scope": "all", "unknown_field": "x"`},
		{reasonIdentity, `"name": "demo"`, `"name": "other"`},
		{reasonAccess, `"receive": false`, `"receive": false, "send": ["cursor:b", "cursor:a"]`},
		{reasonRevision, `"revision": "[0-9a-f]+"`, `"revision": "` + strings.Repeat("0", 64) + `"`},
		{reasonEpoch, `"epoch": "[0-9a-f-]+"`, `"epoch": ""`},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			s := fixtureStore(t)
			f := installForTest(t, s)
			rewriteRecord(t, f, tc.field, tc.value)
			_, err := f.PluginStatus()
			var reasoned interface{ LogReason() string }
			if err == nil || err.Error() != "plugin record demo cannot be read; remove it with: zalki plugins remove demo" {
				t.Fatalf("status error = %v, want the plain error", err)
			}
			if !errors.As(err, &reasoned) || reasoned.LogReason() != tc.reason {
				t.Fatalf("status error has no reason %q", tc.reason)
			}
			if err := f.RemovePlugin(); err != nil {
				t.Fatalf("remove failed: %v", err)
			}
			if status, err := f.PluginStatus(); err != nil || status.ID != "" {
				t.Fatalf("after remove: id %q, error %v", status.ID, err)
			}
			installForTest(t, s) // the name is free again
		})
	}
}

// rewriteRecord replaces the one match of the pattern in the stored record. It
// edits the text, because the receipt's access field must keep its exact bytes.
func rewriteRecord(t *testing.T, f PluginFeed, pattern, replacement string) {
	t.Helper()
	raw, err := os.ReadFile(f.pluginPath())
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(pattern)
	if len(re.FindAll(raw, -1)) != 1 {
		t.Fatalf("pattern %q does not match once in the record", pattern)
	}
	if err := os.WriteFile(f.pluginPath(), re.ReplaceAllLiteral(raw, []byte(replacement)), 0600); err != nil {
		t.Fatal(err)
	}
}

// Remove acts only on files derived from the checked plugin name and, for the
// installation files, from a canonical UUID read from the record. A broken record
// whose id, artifact and executable point outside the state folder never makes
// remove touch them, and a leftover installation file never blocks a reinstall.
func TestRemoveOfAnUnreadableRecordNeverTouchesPathsFromTheRecord(t *testing.T) {
	for _, pathAsID := range []bool{false, true} {
		s := fixtureStore(t)
		f := installForTest(t, s)
		outside, victims := writeBrokenRecordWithOutsidePaths(t, f, pathAsID)
		if _, err := f.PluginStatus(); err == nil || !strings.HasPrefix(err.Error(), "plugin record demo cannot be read") {
			t.Fatalf("path as id %v: status error = %v", pathAsID, err)
		}
		if err := f.RemovePlugin(); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(f.pluginPath()); !os.IsNotExist(err) {
			t.Fatal("remove left the record file")
		}
		assertOutsideUntouched(t, outside, victims)
		installForTest(t, s)
		assertOutsideUntouched(t, outside, victims)
	}
}

// writeBrokenRecordWithOutsidePaths turns the installed record into one that
// cannot be read and points its id, artifact and executable at files in a folder
// outside the state. With pathAsID the id is a relative path that would resolve
// to one of those files under the plugin folder.
func writeBrokenRecordWithOutsidePaths(t *testing.T, f PluginFeed, pathAsID bool) (string, []string) {
	t.Helper()
	outside := t.TempDir()
	victims := []string{"victim.json", "victim.data", "id-victim.json"}
	for _, v := range victims {
		if err := os.WriteFile(filepath.Join(outside, v), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id := model.NewUUID()
	if pathAsID {
		rel, err := filepath.Rel(filepath.Join(f.pluginDir(), "id-a"), outside)
		if err != nil {
			t.Fatal(err)
		}
		id = "a/" + rel + "/victim"
	}
	rewriteRecord(t, f, `"id": "[0-9a-f-]+"`, `"id": "`+id+`"`)
	rewriteRecord(t, f, `"name": "demo"`, `"name": "other"`)
	rewriteRecord(t, f, `"artifact": "plugin"`, `"artifact": "`+filepath.Join(outside, "victim.data")+`"`)
	rewriteRecord(t, f, `"exec": "[^"]*"`, `"exec": "`+filepath.Join(outside, "victim.json")+`"`)
	return outside, victims
}

func assertOutsideUntouched(t *testing.T, outside string, victims []string) {
	t.Helper()
	for _, v := range victims {
		if raw, err := os.ReadFile(filepath.Join(outside, v)); err != nil || string(raw) != "keep" {
			t.Fatalf("something touched %s outside the state folder: %q %v", v, raw, err)
		}
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != len(victims) {
		t.Fatalf("outside folder changed: %v %v", entries, err)
	}
}
