package plugins

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const goodManifest = `{"name":"demo","language":"go","purpose":"p","build":["go","build","-o","plugin","."],"artifact":"plugin","requests":{"feed":true,"read":false,"send":false,"inbox_read":false,"accept_from":false}}`

func TestParseInstallManifestRules(t *testing.T) {
	m, err := parseInstallManifest([]byte(goodManifest))
	if err != nil || m.Args == nil || m.Name != "demo" {
		t.Fatal("valid manifest refused or args not normalized", m, err)
	}
	native := strings.Replace(goodManifest, `"language":"go"`, `"language":"native"`, 1)
	native = strings.Replace(native, `"build":["go","build","-o","plugin","."]`, `"build":null`, 1)
	if _, err := parseInstallManifest([]byte(native)); err != nil {
		t.Fatal("native manifest refused", err)
	}
	bad := map[string]string{
		"unknown field":   strings.Replace(goodManifest, `"name":"demo"`, `"name":"demo","extra":true`, 1),
		"missing build":   strings.Replace(goodManifest, `"build":["go","build","-o","plugin","."],`, ``, 1),
		"missing purpose": strings.Replace(goodManifest, `"purpose":"p",`, ``, 1),
		"null object":     `null`,
		"array":           `[]`,
		"trailing data":   goodManifest + `{}`,
		"duplicate root":  goodManifest + goodManifest,
		"wrong type":      strings.Replace(goodManifest, `"name":"demo"`, `"name":1`, 1),
		"python":          strings.Replace(goodManifest, `"language":"go"`, `"language":"python"`, 1),
		"go without go":   strings.Replace(goodManifest, `["go",`, `["make",`, 1),
		"go run":          strings.Replace(goodManifest, `["go","build",`, `["go","run",`, 1),
		"go gcflags":      strings.Replace(goodManifest, `["go","build",`, `["go","build","-gcflags=x",`, 1),
		"go null build":   strings.Replace(goodManifest, `["go","build","-o","plugin","."]`, `null`, 1),
		"native build":    strings.Replace(goodManifest, `"language":"go"`, `"language":"native"`, 1),
		"escape artifact": strings.Replace(goodManifest, `"artifact":"plugin"`, `"artifact":"../plugin"`, 1),
		"abs artifact":    strings.Replace(goodManifest, `"artifact":"plugin"`, `"artifact":"/bin/sh"`, 1),
		"dirty artifact":  strings.Replace(goodManifest, `"artifact":"plugin"`, `"artifact":"a/./plugin"`, 1),
		"empty artifact":  strings.Replace(goodManifest, `"artifact":"plugin"`, `"artifact":""`, 1),
		"no feed":         strings.Replace(goodManifest, `"feed":true`, `"feed":false`, 1),
		"control purpose": strings.Replace(goodManifest, `"purpose":"p"`, `"purpose":"a\u001bb"`, 1),
		"bidi purpose":    strings.Replace(goodManifest, `"purpose":"p"`, `"purpose":"a\u202eb"`, 1),
		"long purpose":    strings.Replace(goodManifest, `"purpose":"p"`, `"purpose":"`+strings.Repeat("a", 201)+`"`, 1),
		"control arg":     strings.Replace(goodManifest, `"artifact":"plugin"`, `"artifact":"plugin","args":["a\nb"]`, 1),
		"nine args":       strings.Replace(goodManifest, `"artifact":"plugin"`, `"artifact":"plugin","args":["1","2","3","4","5","6","7","8","9"]`, 1),
		"big args":        strings.Replace(goodManifest, `"artifact":"plugin"`, `"artifact":"plugin","args":["`+strings.Repeat("a", 1000)+`","`+strings.Repeat("a", 100)+`"]`, 1),
		"go path tool":    strings.Replace(goodManifest, `"build":["go",`, `"build":["/usr/bin/go",`, 1),
	}
	for name, raw := range bad {
		if _, err := parseInstallManifest([]byte(raw)); err == nil {
			t.Error(name, "accepted")
		}
	}
}

func TestSnapshotInstallSourceDigest(t *testing.T) {
	source := t.TempDir()
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(source, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(installManifestFile, goodManifest, 0644)
	write("main.go", "package main\n", 0644)
	snap := func() InstallSource {
		t.Helper()
		result, err := SnapshotInstallSource(source, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	first, second := snap(), snap()
	if first.TreeSHA256 != second.TreeSHA256 || len(first.TreeSHA256) != 64 || len(first.ManifestSHA256) != 64 {
		t.Fatal("snapshot digest is not stable", first, second)
	}
	if raw, err := os.ReadFile(filepath.Join(first.Dir, "main.go")); err != nil || string(raw) != "package main\n" {
		t.Fatal("copy differs", string(raw), err)
	}
	for _, change := range []func(){
		func() { write("main.go", "package main // x\n", 0644) },
		func() { write("main.go", "package main // x\n", 0755) },
		func() { write("sub/new.txt", "", 0644) },
		func() { write("main.go2", "", 0644) },
	} {
		before := snap().TreeSHA256
		change()
		if after := snap().TreeSHA256; after == before {
			t.Fatal("snapshot digest missed a change")
		}
	}
	if info, err := os.Stat(filepath.Join(snap().Dir, "main.go")); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("executable bit not kept privately", info, err)
	}
}

// The cap counts files and folders; either kind alone can pass it.
func TestSnapshotInstallSourceFileCap(t *testing.T) {
	previous := maxInstallFiles
	maxInstallFiles = 6
	defer func() { maxInstallFiles = previous }()
	for _, folders := range []bool{false, true} {
		big := t.TempDir()
		if err := os.WriteFile(filepath.Join(big, installManifestFile), []byte(goodManifest), 0600); err != nil {
			t.Fatal(err)
		}
		for i := range maxInstallFiles {
			// "zz" sorts after the manifest, so only the folder count can stop it.
			path := filepath.Join(big, "zz"+strconv.Itoa(i))
			if err := map[bool]func() error{
				false: func() error { return os.WriteFile(path, nil, 0600) },
				true:  func() error { return os.Mkdir(path, 0700) },
			}[folders](); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := SnapshotInstallSource(big, t.TempDir()); err == nil || !strings.Contains(err.Error(), "more than 6 files and folders") {
			t.Fatal("cap not enforced, folders:", folders, err)
		}
	}
}

// writeInstallTree writes each name as a file holding the manifest text.
func writeInstallTree(t *testing.T, names ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range names {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(goodManifest), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestSnapshotInstallSourceFileCapNestedAndExact(t *testing.T) {
	previous := maxInstallFiles
	maxInstallFiles = 6
	defer func() { maxInstallFiles = previous }()
	// A file after a full sub-folder is stopped by the per-file check.
	nested := writeInstallTree(t, "a/1", "a/2", "a/3", "a/4", installManifestFile, "zz")
	if _, err := SnapshotInstallSource(nested, t.TempDir()); err == nil || !strings.Contains(err.Error(), "more than 6 files and folders") {
		t.Fatal("cap not enforced after a sub-folder", err)
	}
	// Exactly at the cap is accepted: the root, the manifest and four entries.
	if _, err := SnapshotInstallSource(writeInstallTree(t, installManifestFile, "a", "b", "c", "d"), t.TempDir()); err != nil {
		t.Fatal("source at the cap refused", err)
	}
}

func TestStagedInstallArtifactRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "plugin"), []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "plugin"), filepath.Join(dir, "plugin")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "real"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real", "plugin"), []byte("#!/bin/sh\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"out/plugin", "plugin", "real", "nope"} {
		if _, err := StagedInstallArtifact(dir, rel); err == nil {
			t.Fatal(rel, "accepted")
		}
	}
	if path, err := StagedInstallArtifact(dir, "real/plugin"); err != nil || path != filepath.Join(dir, "real", "plugin") {
		t.Fatal(path, err)
	}
}

func TestRunInstallBuildUsesLiteralArgv(t *testing.T) {
	tool, err := ResolveInstallTool("sh")
	if err != nil || !filepath.IsAbs(tool.Path) || len(tool.SHA256) != 64 {
		t.Fatal(tool, err)
	}
	dir := t.TempDir()
	log := filepath.Join(t.TempDir(), "build.log")
	argv := []string{"sh", "-c", `printf '%s|%s' "$0" "$PWD" > out`, "$HOME"}
	if err := RunInstallBuild(t.Context(), tool, argv, dir, log); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "out"))
	resolved, _ := filepath.EvalSymlinks(dir)
	if err != nil || (string(raw) != "$HOME|"+dir && string(raw) != "$HOME|"+resolved) {
		t.Fatalf("argv was not literal or cwd wrong: %q %v", raw, err)
	}
	failing := filepath.Join(t.TempDir(), "fail.log")
	if err := RunInstallBuild(t.Context(), tool, []string{"sh", "-c", "echo boom; exit 3"}, dir, failing); err == nil || TailInstallLog(failing, 5) != "boom" {
		t.Fatal("failed build not reported with its log", err)
	}
	if _, err := ResolveInstallTool("zalki-no-such-tool"); err == nil || !strings.Contains(err.Error(), `build tool "zalki-no-such-tool" was not found on PATH`) {
		t.Fatal("missing tool resolved")
	}
}

func TestReadInstallManifestAndSourceRefusals(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "real.json")
	if err := os.WriteFile(target, []byte(goodManifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, installManifestFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstallManifest(dir); err == nil {
		t.Fatal("linked manifest accepted")
	}
	big := t.TempDir()
	padded := strings.Replace(goodManifest, `"purpose":"p"`, `"purpose":"p"`+strings.Repeat(" ", 16<<10), 1)
	if err := os.WriteFile(filepath.Join(big, installManifestFile), []byte(padded), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstallManifest(big); err == nil || !strings.Contains(err.Error(), "16 KiB") {
		t.Fatal("oversized manifest accepted", err)
	}
	asDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(asDir, installManifestFile), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadInstallManifest(asDir); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatal("folder manifest accepted", err)
	}
	if _, err := ResolveInstallSource(target); err == nil {
		t.Fatal("a file was accepted as the source folder")
	}
	if got, err := ResolveInstallSource(dir); err != nil || !filepath.IsAbs(got) {
		t.Fatal(got, err)
	}
}

func TestSnapshotInstallSourceByteCap(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, installManifestFile), []byte(goodManifest), 0600); err != nil {
		t.Fatal(err)
	}
	previous := maxInstallBytes
	maxInstallBytes = int64(len(goodManifest)) + 100
	t.Cleanup(func() { maxInstallBytes = previous })
	if _, err := SnapshotInstallSource(source, t.TempDir()); err != nil {
		t.Fatal("source at the cap refused", err)
	}
	if err := os.WriteFile(filepath.Join(source, "huge"), make([]byte, 101), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotInstallSource(source, t.TempDir()); err == nil || !strings.Contains(err.Error(), "MiB") {
		t.Fatal("byte cap not enforced", err)
	}
}

func TestRunInstallBuildStopsAtTimeout(t *testing.T) {
	previous := installTimeout
	installTimeout = 200 * time.Millisecond
	t.Cleanup(func() { installTimeout = previous })
	tool, err := ResolveInstallTool("sh")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err = RunInstallBuild(t.Context(), tool, []string{"sh", "-c", "sleep 30 & sleep 30"}, t.TempDir(), filepath.Join(t.TempDir(), "log"))
	if err == nil || !strings.Contains(err.Error(), "build stopped") || time.Since(start) > 4*time.Second {
		t.Fatal("slow build was not stopped with its children", err, time.Since(start))
	}
}

func TestTailInstallLogKeepsLastLines(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(log, []byte("1\n2\n3\n4\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := TailInstallLog(log, 2); got != "3\n4" {
		t.Fatalf("%q", got)
	}
}

// A parent folder swapped for a link during the walk is refused, whether the
// link points outside the source or to another folder inside it.
func TestSnapshotInstallSourceRefusesSwappedParent(t *testing.T) {
	// os.Root refuses an absolute link as an escape but follows a relative one
	// inside the source, so the inside case needs the SameFile check.
	t.Run("outside", func(t *testing.T) { assertSwappedParentRefused(t, false) })
	t.Run("inside", func(t *testing.T) { assertSwappedParentRefused(t, true) })
}

func assertSwappedParentRefused(t *testing.T, inside bool) {
	t.Helper()
	source := writeInstallTree(t, installManifestFile, "sub/plugin", "other/plugin")
	outside := writeInstallTree(t, "plugin")
	target := outside
	if inside {
		target = "other"
	}
	beforeSnapshotOpen = func(rel string) {
		if rel != "sub" {
			return
		}
		// The original folder is renamed aside (kept alive), so no inode is reused.
		if err := errors.Join(os.Rename(filepath.Join(source, "sub"), filepath.Join(outside, "moved")), os.Symlink(target, filepath.Join(source, "sub"))); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { beforeSnapshotOpen = func(string) {} }()
	staging := t.TempDir()
	_, err := SnapshotInstallSource(source, staging)
	copied, _ := os.ReadFile(filepath.Join(staging, "src", "sub", "plugin"))
	if err == nil || len(copied) != 0 {
		t.Fatalf("swapped parent accepted, copied %q, err %v", copied, err)
	}
}

// A file swapped for a different file after its check is refused. The old file
// is renamed aside, not deleted, so its inode stays in use and cannot be reused
// for the new file (Linux often reuses a freed inode number at once).
func TestSnapshotInstallSourceRefusesSwappedFile(t *testing.T) {
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, installManifestFile), []byte(goodManifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte("checked"), 0600); err != nil {
		t.Fatal(err)
	}
	beforeSnapshotOpen = func(rel string) {
		if rel != "main.go" {
			return
		}
		path, aside := filepath.Join(source, "main.go"), filepath.Join(source, "main.go.old")
		if err := errors.Join(os.Rename(path, aside), os.WriteFile(path, []byte("swapped"), 0600)); err != nil {
			t.Fatal(err)
		}
		old, oldErr := os.Lstat(aside)
		replaced, newErr := os.Lstat(path)
		if oldErr != nil || newErr != nil || os.SameFile(old, replaced) {
			t.Fatal("fixture did not make a file with a different identity", oldErr, newErr)
		}
	}
	defer func() { beforeSnapshotOpen = func(string) {} }()
	if _, err := SnapshotInstallSource(source, t.TempDir()); err == nil || !strings.Contains(err.Error(), "changed while copying") {
		t.Fatal("swapped file accepted", err)
	}
}

// The build runs the reviewed file: a PATH link moved after the review does not
// change what runs, and a change to the reviewed file itself is refused.
func TestRunInstallBuildRunsOnlyTheReviewedTool(t *testing.T) {
	bin := t.TempDir()
	reviewed, replaced := filepath.Join(bin, "go-reviewed"), filepath.Join(bin, "go-replaced")
	for path, word := range map[string]string{reviewed: "reviewed", replaced: "replaced"} {
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf "+word+" > result\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(bin, "go")
	if err := os.Symlink(reviewed, link); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	tool, err := ResolveInstallTool("go")
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := filepath.EvalSymlinks(reviewed); tool.Path != resolved {
		t.Fatal("tool path is not the resolved file", tool.Path)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replaced, link); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := RunInstallBuild(t.Context(), tool, []string{"go"}, dir, filepath.Join(t.TempDir(), "log")); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(filepath.Join(dir, "result")); string(raw) != "reviewed" {
		t.Fatalf("ran %q, not the reviewed tool", raw)
	}
	if err := os.WriteFile(reviewed, []byte("#!/bin/sh\nprintf changed > result\n"), 0700); err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	err = RunInstallBuild(t.Context(), tool, []string{"go"}, dir, filepath.Join(t.TempDir(), "log"))
	if _, statErr := os.Stat(filepath.Join(dir, "result")); err == nil || !strings.Contains(err.Error(), "changed after the review") || statErr == nil {
		t.Fatal("changed tool ran", err, statErr)
	}
}

// A build that writes without end is stopped at the log cap.
func TestRunInstallBuildCapsItsLog(t *testing.T) {
	previousLog, previousTimeout := maxInstallLog, installTimeout
	maxInstallLog, installTimeout = 64<<10, 6*time.Second // the timeout bounds a broken cap
	defer func() { maxInstallLog, installTimeout = previousLog, previousTimeout }()
	tool, err := ResolveInstallTool("sh")
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "build.log")
	start := time.Now()
	err = RunInstallBuild(t.Context(), tool, []string{"sh", "-c", "while :; do echo noisy build output line; done"}, t.TempDir(), log)
	info, statErr := os.Stat(log)
	if err == nil || !strings.Contains(err.Error(), "build output passed") || statErr != nil || info.Size() != maxInstallLog || time.Since(start) > 4*time.Second {
		t.Fatal("noisy build not stopped at the cap", err, info, statErr, time.Since(start))
	}
	if tail := TailInstallLog(log, 3); !strings.Contains(tail, "noisy build output line") || len(tail) > 200 {
		t.Fatal("log tail is not short and readable", tail)
	}
}

// A link swapped in that leads back to the same folder inside the source keeps
// the folder identity, so it is not detected; the same bytes are copied and
// nothing outside the source is read. This documents the real guarantee.
func TestSnapshotInstallSourceSameFolderLinkCopiesSameBytes(t *testing.T) {
	source := t.TempDir()
	for path, body := range map[string]string{installManifestFile: goodManifest, "sub/plugin": "original"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(source, path)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, path), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := SnapshotInstallSource(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	beforeSnapshotOpen = func(rel string) {
		if rel != "sub" {
			return
		}
		if err := errors.Join(os.Rename(filepath.Join(source, "sub"), filepath.Join(source, "moved")), os.Symlink("moved", filepath.Join(source, "sub"))); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { beforeSnapshotOpen = func(string) {} }()
	got, err := SnapshotInstallSource(source, t.TempDir())
	if err != nil || got.TreeSHA256 != want.TreeSHA256 {
		t.Fatal("same-folder link changed the copy", err, got.TreeSHA256, want.TreeSHA256)
	}
}

// A huge folder is read in chunks and refused at the cap, not read whole.
func TestSnapshotInstallSourceReadsWideFoldersInChunks(t *testing.T) {
	previous, previousRead := maxInstallFiles, readInstallNames
	maxInstallFiles = 4
	read := 0
	readInstallNames = func(list *os.File) ([]string, error) {
		names, err := previousRead(list)
		read += len(names)
		return names, err
	}
	defer func() { maxInstallFiles, readInstallNames = previous, previousRead }()
	wide := t.TempDir()
	for i := range 300 {
		if err := os.WriteFile(filepath.Join(wide, "f"+strconv.Itoa(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := SnapshotInstallSource(wide, t.TempDir()); err == nil || !strings.Contains(err.Error(), "more than 4") || read > 256 {
		t.Fatal("wide folder not stopped after the first chunk", err, read)
	}
}

// A build whose leader exits at once must not leave a child behind, and the
// child cannot write past the cap after the leader is gone.
func TestRunInstallBuildStopsChildAfterLeaderExit(t *testing.T) {
	cases := []struct {
		name, child, want string
		wait              time.Duration
		limit             int64
	}{
		{"burst past cap", "sleep 0.3; i=0; while [ $i -lt 40 ]; do printf '%0200d\\n' 0; i=$((i+1)); done; sleep 8", "build output passed", 2 * time.Second, 1 << 10},
		{"holds output", "sleep 8", "background process", 300 * time.Millisecond, 1 << 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			previousLog, previousWait := maxInstallLog, installWaitDelay
			maxInstallLog, installWaitDelay = c.limit, c.wait
			defer func() { maxInstallLog, installWaitDelay = previousLog, previousWait }()
			dir := t.TempDir()
			script := "trap '' PIPE\necho $$ > child.pid\n" + c.child + "\n"
			if err := os.WriteFile(filepath.Join(dir, "child.sh"), []byte(script), 0600); err != nil {
				t.Fatal(err)
			}
			tool, err := ResolveInstallTool("sh")
			if err != nil {
				t.Fatal(err)
			}
			log := filepath.Join(t.TempDir(), "build.log")
			start := time.Now()
			err = RunInstallBuild(t.Context(), tool, []string{"sh", "-c", "sh ./child.sh & exit 0"}, dir, log)
			took := time.Since(start)
			raw, _ := os.ReadFile(filepath.Join(dir, "child.pid"))
			pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
			t.Cleanup(func() {
				if pid > 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			})
			info, statErr := os.Stat(log)
			if err == nil || !strings.Contains(err.Error(), c.want) || took > c.wait+2*time.Second || statErr != nil || info.Size() > c.limit {
				t.Fatal("build with a lingering child not stopped cleanly", err, took, info, statErr)
			}
			if pid <= 0 || !processGone(pid, 2*time.Second) {
				t.Fatal("child still running after the build returned", pid)
			}
		})
	}
}

func processGone(pid int, within time.Duration) bool {
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}
