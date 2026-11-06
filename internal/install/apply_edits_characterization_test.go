package install

// TEMPORARY REFACTOR GUARD. Unlike the other characterization tests in this
// package, this file is in-package: applyConfigEdits has no public seam that can
// reach its "changed during planning" refusal or a failure in the middle of the
// writes, because both depend on timing after the plan is made. It is therefore
// tied to the signature applyConfigEdits([]configEdit) error and to the
// configEdit fields path, before, after, existed and remove. If a later change
// reshapes them, adapt this file deliberately, in the same reviewed change,
// and keep the assertions: they describe behavior, not structure.
// Preparation can leave backups or directories after a later refusal; the
// assertions protect target-file contents without endorsing those artifacts.
// Rollback failure and joined rollback errors are not covered by this fixture.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func writePlain(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return err == nil
}

// TestApplyConfigEditsRefusesWhenTheWorldChangedSincePlanning pins the check
// that guards the user's files: if a file differs from what the plan saw, the
// whole request is refused with a message naming the file, and no settings
// file is modified.
func TestApplyConfigEditsRefusesWhenTheWorldChangedSincePlanning(t *testing.T) {
	tests := []struct {
		name string
		// culprit is the file whose change the refusal must name: "a" or "b".
		culprit string
		setup   func(t *testing.T, a, b string) []configEdit
	}{
		{"the second file's bytes changed", "b", func(t *testing.T, a, b string) []configEdit {
			t.Helper()
			writePlain(t, a, "a-old")
			writePlain(t, b, "b-edited-by-the-user")
			return []configEdit{
				{path: a, before: []byte("a-old"), after: []byte("a-new"), existed: true},
				{path: b, before: []byte("b-old"), after: []byte("b-new"), existed: true},
			}
		}},
		{"the first file's bytes changed", "a", func(t *testing.T, a, b string) []configEdit {
			t.Helper()
			writePlain(t, a, "a-edited-by-the-user")
			writePlain(t, b, "b-old")
			return []configEdit{
				{path: a, before: []byte("a-old"), after: []byte("a-new"), existed: true},
				{path: b, before: []byte("b-old"), after: []byte("b-new"), existed: true},
			}
		}},
		{"a file the plan found missing now exists", "b", func(t *testing.T, a, b string) []configEdit {
			t.Helper()
			writePlain(t, a, "a-old")
			writePlain(t, b, "someone created this")
			return []configEdit{
				{path: a, before: []byte("a-old"), after: []byte("a-new"), existed: true},
				{path: b, after: []byte("b-new")},
			}
		}},
		{"a file the plan found has vanished", "b", func(t *testing.T, a, b string) []configEdit {
			t.Helper()
			writePlain(t, a, "a-old")
			return []configEdit{
				{path: a, before: []byte("a-old"), after: []byte("a-new"), existed: true},
				{path: b, before: []byte("b-old"), after: []byte("b-new"), existed: true},
			}
		}},
		{"an empty file the plan saw has vanished", "b", func(t *testing.T, a, b string) []configEdit {
			t.Helper()
			writePlain(t, a, "a-old")
			return []configEdit{
				{path: a, before: []byte("a-old"), after: []byte("a-new"), existed: true},
				{path: b, before: []byte{}, after: []byte("b-new"), existed: true},
			}
		}},
		{"an empty file appeared where the plan saw none", "b", func(t *testing.T, a, b string) []configEdit {
			t.Helper()
			writePlain(t, a, "a-old")
			writePlain(t, b, "")
			return []configEdit{
				{path: a, before: []byte("a-old"), after: []byte("a-new"), existed: true},
				{path: b, after: []byte("b-new")},
			}
		}},
		{"a removal whose file changed", "b", func(t *testing.T, a, b string) []configEdit {
			t.Helper()
			writePlain(t, a, "a-old")
			writePlain(t, b, "b-edited-by-the-user")
			return []configEdit{
				{path: a, before: []byte("a-old"), after: []byte("a-new"), existed: true},
				{path: b, before: []byte("b-old"), existed: true, remove: true},
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			a, b := filepath.Join(dir, "a.json"), filepath.Join(dir, "sub", "b.json")
			edits := test.setup(t, a, b)
			beforeA, aExisted := "", exists(t, a)
			if aExisted {
				beforeA = mustRead(t, a)
			}
			beforeB, bExisted := "", exists(t, b)
			if bExisted {
				beforeB = mustRead(t, b)
			}

			err := applyConfigEdits(edits)
			culprit := map[string]string{"a": a, "b": b}[test.culprit]
			want := culprit + " changed during planning; rerun the command"
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			// The settings files are exactly as the user left them.
			if exists(t, a) != aExisted || (aExisted && mustRead(t, a) != beforeA) {
				t.Errorf("%s was modified by a refused request", a)
			}
			if exists(t, b) != bExisted || (bExisted && mustRead(t, b) != beforeB) {
				t.Errorf("%s was modified by a refused request", b)
			}
		})
	}
}

func TestApplyConfigEditsRefusesTargetsItCannotUse(t *testing.T) {
	t.Run("a target that is not a regular file", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "settings.json")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		err := applyConfigEdits([]configEdit{{path: target, after: []byte("x")}})
		if err == nil || err.Error() != "refusing non-regular or oversized settings file" {
			t.Fatalf("error = %v", err)
		}
	})

	t.Run("a parent that is a file", func(t *testing.T) {
		dir := t.TempDir()
		parent := filepath.Join(dir, "parent")
		writePlain(t, parent, "i am a file")
		err := applyConfigEdits([]configEdit{{path: filepath.Join(parent, "settings.json"), after: []byte("x")}})
		if !errors.Is(err, syscall.ENOTDIR) {
			t.Fatalf("error = %v, want ENOTDIR", err)
		}
		if mustRead(t, parent) != "i am a file" {
			t.Error("the file in the way was modified")
		}
	})
}

// TestApplyConfigEditsStopsBeforeWritingWhenItCannotPrepare pins failures in
// the preparation pass: no parent directory can be created, or the backup of an
// existing file cannot be written. The settings file keeps its old bytes.
func TestApplyConfigEditsStopsBeforeWritingWhenItCannotPrepare(t *testing.T) {
	lock := func(t *testing.T, dir string) {
		t.Helper()
		if os.Geteuid() == 0 {
			t.Skip("directory permissions do not apply to root")
		}
		if err := os.Chmod(dir, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	}
	t.Run("the parent directory cannot be created", func(t *testing.T) {
		dir := t.TempDir()
		lock(t, dir)
		target := filepath.Join(dir, "missing", "settings.json")
		err := applyConfigEdits([]configEdit{{path: target, after: []byte("x")}})
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("error = %v, want a permission error", err)
		}
		if exists(t, filepath.Join(dir, "missing")) {
			t.Error("a directory was created")
		}
	})
	t.Run("the backup name is too long although the file name is not", func(t *testing.T) {
		// A backup adds a suffix to the file name; a name close to the
		// 255-byte limit fits for the file but not for its backup. The write
		// itself would succeed, so only the backup step can refuse.
		dir := t.TempDir()
		target := filepath.Join(dir, strings.Repeat("n", 230))
		writePlain(t, target, "old")
		err := applyConfigEdits([]configEdit{{path: target, before: []byte("old"), after: []byte("new"), existed: true}})
		if !errors.Is(err, syscall.ENAMETOOLONG) {
			t.Fatalf("error = %v, want ENAMETOOLONG from the backup", err)
		}
		if got := mustRead(t, target); got != "old" {
			t.Errorf("settings hold %q after a failed backup, want %q", got, "old")
		}
	})
	t.Run("the backup cannot be written", func(t *testing.T) {
		dir := t.TempDir()
		target := filepath.Join(dir, "settings.json")
		writePlain(t, target, "old")
		lock(t, dir)
		err := applyConfigEdits([]configEdit{{path: target, before: []byte("old"), after: []byte("new"), existed: true}})
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("error = %v, want a permission error", err)
		}
		if got := mustRead(t, target); got != "old" {
			t.Errorf("settings hold %q after a failed backup, want %q", got, "old")
		}
	})
}

// TestApplyConfigEditsRollsBackAfterAWriteFailure checks target-file restoration
// for this write failure: existing files get their old bytes back and files
// this request created are removed. It does not assert backup/directory cleanup
// or successful recovery when rollback itself fails.
func TestApplyConfigEditsRollsBackAfterAWriteFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("directory permissions do not apply to root")
	}
	dir := t.TempDir()
	existing := filepath.Join(dir, "existing.json")
	created := filepath.Join(dir, "created", "new.json")
	locked := filepath.Join(dir, "locked")
	failing := filepath.Join(locked, "never.json")
	writePlain(t, existing, "old")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0500); err != nil { // Nothing can be created inside.
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })

	err := applyConfigEdits([]configEdit{
		{path: existing, before: []byte("old"), after: []byte("new"), existed: true},
		{path: created, after: []byte("created")},
		{path: failing, after: []byte("never")},
	})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("error = %v, want a permission error", err)
	}
	if got := mustRead(t, existing); got != "old" {
		t.Errorf("existing file holds %q after the rollback, want %q", got, "old")
	}
	if exists(t, created) {
		t.Error("a file created by the failed request was not removed")
	}
	if exists(t, failing) {
		t.Error("the failing target exists")
	}
}

// TestApplyConfigEditsAppliesEditsInOrder pins the success contract in the
// same terms: new content, removal, and creation of missing parent directories.
func TestApplyConfigEditsAppliesEditsInOrder(t *testing.T) {
	dir := t.TempDir()
	changed := filepath.Join(dir, "changed.json")
	removed := filepath.Join(dir, "removed.json")
	created := filepath.Join(dir, "deep", "er", "created.json")
	writePlain(t, changed, "old")
	writePlain(t, removed, "gone-soon")

	err := applyConfigEdits([]configEdit{
		{path: changed, before: []byte("old"), after: []byte("new"), existed: true},
		{path: removed, before: []byte("gone-soon"), existed: true, remove: true},
		{path: created, after: []byte("fresh")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if mustRead(t, changed) != "new" || exists(t, removed) || mustRead(t, created) != "fresh" {
		t.Errorf("results: changed=%q removed exists=%v created=%q", mustRead(t, changed), exists(t, removed), mustRead(t, created))
	}
	if info, err := os.Stat(filepath.Dir(created)); err != nil || info.Mode().Perm() != 0700 {
		t.Errorf("created parent directory: %v %v, want mode 0700", info, err)
	}
}
