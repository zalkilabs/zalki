package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// ZAL-85: doctor has no plugin section, so a plugin record or folder that
// cannot be read is named with its cause and its file, never "run zalki
// doctor", and the record's content is not shown.
func TestPluginUnreadableRecordNamesCause(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	installReviewInstance(t, store.Dir, "demo", "0.1.0")
	record := filepath.Join(store.Dir, "plugins", "demo.json")
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	// Valid JSON, not a valid record for demo: another plugin's name.
	edited := regexp.MustCompile(`"name":\s*"demo"`).ReplaceAll(raw, []byte(`"name":"other"`))
	if string(edited) == string(raw) {
		t.Fatal("the test could not edit the record")
	}
	if err := os.WriteFile(record, edited, 0o600); err != nil {
		t.Fatal(err)
	}
	cause := "its content is not a valid plugin record; the record is "
	out, err := pluginOwnerLocal(t, store.Dir, "show", "demo")
	if err == nil || !strings.HasPrefix(err.Error(), `cannot safely read plugin "demo": `) || !pluginDiagnosticOutputMatches(t, err.Error(), record, `cannot safely read plugin "demo": `+cause, "") {
		t.Fatalf("show dropped the cause of an unreadable record: %q %v", out, err)
	}
	out, err = pluginOwnerLocal(t, store.Dir, "list")
	if err != nil || !strings.Contains(string(out), "demo  unreadable: its content") || !pluginDiagnosticOutputMatches(t, string(out), record, "Unreadable plugin records\n  demo  unreadable: "+cause, "\n") || strings.Contains(string(out), "doctor") {
		t.Fatalf("list did not name the cause of an unreadable record: %q %v", out, err)
	}
	checkPluginsFolderNamed(t)
	checkRecordContentNotEchoed(t)
}

// checkRecordContentNotEchoed: the strict decoder names an unknown field in
// its error; neither that name nor its value reaches show or list, and a
// session-list error is never echoed (ZAL-85 M1).
func checkRecordContentNotEchoed(t *testing.T) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	installReviewInstance(t, store.Dir, "demo", "0.1.0")
	record := filepath.Join(store.Dir, "plugins", "demo.json")
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	marked := regexp.MustCompile(`^\s*\{`).ReplaceAll(raw, []byte(`{"zz_marker_7f3a":"zz_value_9c1d",`))
	if string(marked) == string(raw) {
		t.Fatal("the test could not mark the record")
	}
	if err := os.WriteFile(record, marked, 0o600); err != nil {
		t.Fatal(err)
	}
	show, showErr := pluginOwnerLocal(t, store.Dir, "show", "demo")
	list, listErr := pluginOwnerLocal(t, store.Dir, "list")
	all := string(show) + string(list) + fmt.Sprint(showErr) + fmt.Sprint(listErr)
	if showErr == nil || strings.Contains(all, "zz_marker") || strings.Contains(all, "zz_value") || !strings.Contains(all, "its content is not a valid plugin record") {
		t.Fatalf("record content reached the owner line: %q", all)
	}
	listed := sessionListProblem(errors.New(`session "zz_marker_7f3a": bad`), 0).Error()
	if strings.Contains(listed, "zz_marker") || listed != "could not safely list running sessions: the session records could not be read" {
		t.Fatalf("a session-list error was echoed: %q", listed)
	}
}

// checkPluginsFolderNamed: a file where the plugins folder should be is named.
func checkPluginsFolderNamed(t *testing.T) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	dir := filepath.Join(store.Dir, "plugins")
	_ = os.MkdirAll(dir, 0o700)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := pluginOwnerLocal(t, store.Dir, "list")
	if err == nil || !pluginDiagnosticOutputMatches(t, err.Error(), dir, "the plugins folder ", " is not a safe folder (it is a link or a file)") || strings.Contains(err.Error(), "doctor") {
		t.Fatalf("list did not name an unsafe plugins folder: %q %v", out, err)
	}
	if _, _, err := installedPlugins(store); err == nil || !strings.Contains(err.Error(), "plugins") || strings.Contains(err.Error(), "doctor") {
		t.Fatal("login-item setup did not name its plugin-folder read failure", err)
	}
}

// The command can retain the configured alias or use its resolved parent.
// Match the complete message for only those two paths; never follow the final
// record symlink or accept a basename, substring, or a different cause.
func pluginDiagnosticOutputMatches(t *testing.T, got, path, prefix, suffix string) bool {
	t.Helper()
	if got == prefix+shellQuote(path)+suffix {
		return true
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	return got == prefix+shellQuote(filepath.Join(dir, filepath.Base(path)))+suffix
}

func TestPluginDiagnosticOutputMatchesOnlyEquivalentPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real with 'quotes'")
	alias := filepath.Join(root, "alias with 'quotes'")
	if err := os.MkdirAll(filepath.Join(real, "plugins"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(alias, "plugins", "demo.json")
	resolved := filepath.Join(real, "plugins", "demo.json")
	outside := filepath.Join(root, "outside", "demo.json")
	if err := os.Symlink(outside, record); err != nil {
		t.Fatal(err)
	}
	const prefix = "demo  unreadable: it is not a safe regular file; the record is "
	const suffix = "\n"
	for _, path := range []string{record, resolved} {
		if !pluginDiagnosticOutputMatches(t, prefix+shellQuote(path)+suffix, record, prefix, suffix) {
			t.Fatal("exact configured or parent-resolved path was rejected", path)
		}
	}
	for _, got := range []string{
		prefix + shellQuote(outside) + suffix,
		prefix + shellQuote(filepath.Join(real, "plugins", "other.json")) + suffix,
		prefix + "demo.json" + suffix,
		prefix + record + suffix,
		"a different cause; the record is " + shellQuote(record) + suffix,
		prefix + shellQuote(record) + " extra" + suffix,
	} {
		if pluginDiagnosticOutputMatches(t, got, record, prefix, suffix) {
			t.Fatal("non-equivalent path, quoting, filename, cause, or suffix was accepted", got)
		}
	}
}

// Decoder field/value strings and filesystem error paths are untrusted. Keep
// their diagnostic classes while preserving the former wrapper's error code.
func TestPluginDiagnosticCausesStayBounded(t *testing.T) {
	const private = "private-diagnostic-sentinel"
	store := bus.Store{Dir: filepath.Join(t.TempDir(), "state with 'quotes'")}
	for _, tc := range []struct {
		name, cause string
		err         error
	}{
		{"unsafe", "it is not a safe regular file", errors.Join(journal.ErrInvalidFile, errors.New(private))},
		{"permission", "permission denied", &os.PathError{Op: private, Path: private, Err: os.ErrPermission}},
		{"read", "it could not be read", &os.PathError{Op: private, Path: private, Err: os.ErrNotExist}},
		{"type", "its content is not a valid plugin record", &json.UnmarshalTypeError{Value: private, Type: reflect.TypeOf(true), Struct: private, Field: private}},
		{"unknown", "its content is not a valid plugin record", errors.New(private)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := pluginRecordProblem(store, "demo", tc.err)
			if !strings.Contains(err.Error(), tc.cause) || !strings.Contains(err.Error(), shellQuote(pluginRecordPath(store, "demo"))) || strings.Contains(err.Error(), private) {
				t.Fatal("diagnostic lost its bounded cause or exposed error content", err)
			}
			if model.ErrorCodeOf(err) != model.CodeInternal {
				t.Fatal("display wrapper changed the existing error code", err)
			}
		})
	}
	if got := sessionListProblem(nil, 2).Error(); !strings.Contains(got, "2 session records") || !strings.Contains(got, "zalki doctor") {
		t.Fatal("skipped session records lost their supported diagnostic route", got)
	}
}

func TestPluginInvalidNameDiagnosticDoesNotConstructARecordPath(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	const want = "cannot safely read plugin: " + invalidPluginNameDetail
	for _, tc := range []struct{ label, name string }{
		{"traversal", "../../private-record-sentinel"},
		{"escape", "demo\x1b[31m"},
		{"newline", "demo\nprivate-record-sentinel"},
		{"uppercase", "Demo"},
		{"long", strings.Repeat("a", 33)},
		{"empty", ""},
	} {
		t.Run(tc.label, func(t *testing.T) {
			err := pluginRecordProblem(store, tc.name, errors.New("private-underlying-error"))
			if err.Error() != want || model.ErrorCodeOf(err) != model.CodeInternal {
				t.Fatal("invalid-name helper invented a path or changed its error code", err)
			}
			if got := pluginRecordDetail(store, tc.name, "private-cause-sentinel"); got != invalidPluginNameDetail {
				t.Fatal("invalid-name detail constructed a path or echoed content", got)
			}
			out, err := pluginOwnerLocal(t, store.Dir, "show", "--", tc.name)
			if err == nil || err.Error() != want || len(out) != 0 || model.ErrorCodeOf(err) != model.CodeInternal {
				t.Fatalf("show exposed an invalid name/path or changed its refusal: %q %v", out, err)
			}
		})
	}
}
