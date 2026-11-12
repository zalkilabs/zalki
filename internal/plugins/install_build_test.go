package plugins

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestGoBuildArgvAllowsOnlyTheListedForms(t *testing.T) {
	allowed := [][]string{
		// The four local plugin manifests.
		{"go", "build", "-trimpath", "-o", "plugin", "."},
		{"go", "build", "-p=1", "-mod=readonly", "-trimpath", "-o", "plugin", "./cmd/spawner"},
		{"go", "build", "-mod=readonly", "-trimpath", "-o", "plugin", "."},
		// Each other allowed form.
		{"go", "build", "-o=plugin", "-p=2", "./cmd/a/b"},
		{"go", "build", "-ldflags=-s -w", "."},
		{"go", "build", "-ldflags", "-s -w", "."},
		{"go", "build"},
	}
	for _, argv := range allowed {
		if err := validateGoBuild(argv); err != nil {
			t.Error("allowed build refused", argv, err)
		}
	}
	refused := map[string][]string{
		"go":                 {"go"},
		"run":                {"go", "run", "."},
		"generate":           {"go", "generate", "./..."},
		"-C before build":    {"go", "-C", "/tmp", "build", "."},
		"-C":                 {"go", "build", "-C", "/tmp", "."},
		"-toolexec":          {"go", "build", "-toolexec=/tmp/x", "."},
		"-exec":              {"go", "build", "-exec", "/tmp/x", "."},
		"-overlay":           {"go", "build", "-overlay=/tmp/o.json", "."},
		"-modfile":           {"go", "build", "-modfile=/tmp/go.mod", "."},
		"gcflags -o":         {"go", "build", "-gcflags=all=-o=/outside/file", "-o", "plugin", "."},
		"gcflags asmhdr":     {"go", "build", "-gcflags=all=-asmhdr=/outside/file", "."},
		"gcflags @file":      {"go", "build", "-gcflags=all=@flags.txt", "."},
		"ldflags -o":         {"go", "build", "-ldflags=-o=/outside/file", "."},
		"ldflags extld":      {"go", "build", "-ldflags", "-extld=/tmp/cc", "."},
		"ldflags -X":         {"go", "build", "-ldflags=-X main.v=1", "."},
		"-p=3":               {"go", "build", "-p=3", "."},
		"-mod=mod":           {"go", "build", "-mod=mod", "."},
		"-buildvcs=true":     {"go", "build", "-buildvcs=true", "."},
		"-o outside":         {"go", "build", "-o", "../plugin", "."},
		"-o absolute":        {"go", "build", "-o=/tmp/plugin", "."},
		"-o folder":          {"go", "build", "-o", "bin/plugin", "."},
		"-o last":            {"go", "build", "-o"},
		"-o parent":          {"go", "build", "-o", "..", "."},
		"absolute file":      {"go", "build", "/outside/main.go"},
		"parent folder":      {"go", "build", "../outside"},
		"inner parent":       {"go", "build", "./a/../../outside"},
		"pattern":            {"go", "build", "./..."},
		"go file":            {"go", "build", "./main.go"},
		"import path":        {"go", "build", "example.com/tool"},
		"version":            {"go", "build", "./cmd@v1"},
		"double dash flag":   {"go", "build", "--trimpath", "."},
		"bare relative dir":  {"go", "build", "cmd/spawner"},
		"-compiler":          {"go", "build", "-compiler=gccgo", "."},
		"-gccgoflags":        {"go", "build", "-gccgoflags=-x", "."},
		"--toolexec equals":  {"go", "build", "--toolexec=/tmp/x", "."},
		"gcflags cpuprof":    {"go", "build", "-gcflags=all=-cpuprofile=/outside/p", "."},
		"-pkgdir":            {"go", "build", "-pkgdir=/outside", "."},
		"-pgo":               {"go", "build", "-pgo=/outside/profile", "."},
		"-debug-actiongraph": {"go", "build", "-debug-actiongraph=/outside/file", "."},
		"-modcacherw":        {"go", "build", "-modcacherw", "."},
		"top-level @file":    {"go", "build", "@flags.txt"},
		"linkmode":           {"go", "build", "-ldflags=-linkmode=external", "."},
	}
	for name, argv := range refused {
		err := validateGoBuild(argv)
		if err == nil {
			t.Error(name, "accepted", argv)
		} else if len(argv) > 2 && argv[1] == "build" && !strings.Contains(err.Error(), "is not allowed") {
			t.Error(name, "error does not name the argument", err)
		}
	}
	if err := validateGoBuild([]string{"go", "build", "-trimpath", "-gcflags=x", "-toolexec=y"}); err == nil || !strings.Contains(err.Error(), `build: "-gcflags=x" is not allowed`) {
		t.Error("error does not name the first refused argument", err)
	}
}

func TestGoBuildAlwaysTurnsOffVCSStampingAndVendoring(t *testing.T) {
	if got := goBuildArgs([]string{"go", "build", "-o", "plugin", "."}); !slices.Equal(got, []string{"build", "-buildvcs=false", "-mod=readonly", "-o", "plugin", "."}) {
		t.Fatal(got)
	}
	if got := goBuildArgs([]string{"sh", "-c", "x"}); !slices.Equal(got, []string{"-c", "x"}) {
		t.Fatal(got)
	}
}

func TestRunInstallBuildUsesOnlyTheGoBuildEnv(t *testing.T) {
	for _, kv := range []string{"GOFLAGS=-toolexec=/tmp/x", "GOTOOLCHAIN=go1.99.0", "GOENV=/tmp/go.env", "CGO_ENABLED=1", "GOCACHEPROG=/tmp/x", "GOAUTH=/tmp/x", "GOWORK=/tmp/go.work", "GOROOT=/tmp/goroot", "GOPROXY=https://example.com", "ZALKI_TEST_SECRET=1"} {
		name, value, _ := strings.Cut(kv, "=")
		t.Setenv(name, value)
	}
	tool, err := ResolveInstallTool("sh")
	if err != nil {
		t.Fatal(err)
	}
	dir, staging := t.TempDir(), t.TempDir()
	if err := RunInstallBuild(t.Context(), tool, []string{"sh", "-c", "env > out; test -d \"$HOME\" && test -d \"$TMPDIR\""}, dir, filepath.Join(staging, "log")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "out"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, kv := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		name, value, _ := strings.Cut(kv, "=")
		got[name] = value
	}
	want := map[string]string{"GOTOOLCHAIN": "local", "GOENV": "off", "GOFLAGS": "", "GOWORK": "off", "CGO_ENABLED": "0", "GOPROXY": "off", "GOSUMDB": "off", "GOMAXPROCS": "2", "PATH": os.Getenv("PATH")}
	for name, value := range want {
		if v, ok := got[name]; !ok || v != value {
			t.Errorf("build env %s=%q, want %q", name, v, value)
		}
	}
	for _, name := range []string{"HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH"} {
		if !strings.HasPrefix(got[name], filepath.Join(staging, ".build-")) {
			t.Errorf("build env %s=%q is not in the private build folder", name, got[name])
		}
	}
	for name := range got {
		if _, ok := want[name]; !ok && !slices.Contains([]string{"HOME", "TMPDIR", "GOCACHE", "GOMODCACHE", "GOPATH", "PWD", "SHLVL", "_", "OLDPWD"}, name) {
			t.Errorf("build env kept %s from the owner's environment", name)
		}
	}
	if left, _ := filepath.Glob(filepath.Join(staging, ".build-*")); len(left) != 0 {
		t.Error("private build folder left behind", left)
	}
}

func TestSnapshotSkipsVCSFolders(t *testing.T) {
	source := t.TempDir()
	for _, name := range []string{"zalki-plugin.json", ".git/config", "sub/.hg/hgrc", "sub/.svn/x", ".bzr/x", "sub/main.go"} {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		body := "x"
		if name == "zalki-plugin.json" {
			body = goodManifest
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	src, err := SnapshotInstallSource(source, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".git", "sub/.hg", "sub/.svn", ".bzr"} {
		if _, err := os.Lstat(filepath.Join(src.Dir, name)); !os.IsNotExist(err) {
			t.Error("VCS folder copied", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(src.Dir, "sub/main.go")); err != nil {
		t.Error("source file not copied", err)
	}
}

func TestGoModuleMustUseOnlyTheStandardLibrary(t *testing.T) {
	allowed := map[string]string{
		"dashboard": "module example.invalid/zalki-plugins/dashboard\n\ngo 1.26\n",
		"spawner":   "module local/spawner\n\ngo 1.26.0\n",
		"full":      "// a plugin\nmodule example.com/p // name\n\ngo 1.26\ntoolchain go1.26.10\ngodebug default=go1.21\ngodebug (\n\tpanicnil=1\n\tasynctimerchan=0\n)\n",
		"quoted":    "module \"example.com/p\"\ngo 1.26\n",
	}
	for name, mod := range allowed {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0600); err != nil {
			t.Fatal(err)
		}
		if err := checkGoModule(dir); err != nil {
			t.Error(name, "refused", err)
		}
	}
	refused := map[string]string{
		"replace /outside": "module m\ngo 1.26\nrequire example.org/hidden v0.0.0\nreplace example.org/hidden => /outside/hidden\n",
		"replace ../":      "module m\ngo 1.26\nreplace example.org/hidden v0.0.0 => ../outside\n",
		"replace block":    "module m\ngo 1.26\nreplace (\n\texample.org/hidden => ../outside\n)\n",
		"require block":    "module m\ngo 1.26\nrequire (\n\texample.org/x v1.0.0\n)\n",
		"require paren":    "module m\ngo 1.26\nrequire(\n\texample.org/x v1.0.0\n)\n",
		"tool":             "module m\ngo 1.26\ntool example.org/x/cmd\n",
		"exclude":          "module m\ngo 1.26\nexclude example.org/x v1.0.0\n",
		"retract":          "module m\ngo 1.26\nretract v1.0.0\n",
		"ignore":           "module m\ngo 1.26\nignore ./x\n",
		"after a block":    "module m\ngodebug (\n\tpanicnil=1\n)\nreplace a => ../b\n",
	}
	for name, mod := range refused {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0600); err != nil {
			t.Fatal(err)
		}
		if err := checkGoModule(dir); err == nil || !strings.Contains(err.Error(), "plugins must use only the Go standard library: go.mod has a") {
			t.Error(name, "accepted", err)
		}
	}
	nested := t.TempDir()
	if err := os.MkdirAll(filepath.Join(nested, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "sub", "go.mod"), []byte("module m\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkGoModule(nested); err == nil || !strings.Contains(err.Error(), "needs a go.mod") {
		t.Error("a go.mod only in a sub folder was accepted", err)
	}
	if err := os.WriteFile(filepath.Join(nested, "go.mod"), []byte("module m\ngo 1.26\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkGoModule(nested); err != nil {
		t.Error("root go.mod with a nested module refused", err)
	}
}

func TestRunInstallBuildChecksTheGoModuleFirst(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	fake := filepath.Join(t.TempDir(), "go")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	sum, err := hashInstallTool(fake)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module m\nreplace a => ../b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	err = RunInstallBuild(t.Context(), InstallTool{Path: fake, SHA256: sum}, []string{"go", "build", "."}, dir, filepath.Join(t.TempDir(), "log"))
	if err == nil || !strings.Contains(err.Error(), `go.mod has a "replace" directive`) {
		t.Fatal("build ran with a replace directive", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the go tool ran", err)
	}
}

// TestRunInstallBuildIgnoresAVendorFolder builds a go 1.20 module whose only
// import is a package under vendor/ with no require. The control build, with
// the same environment but without the host flags, uses the vendor folder;
// RunInstallBuild must not.
func TestRunInstallBuildIgnoresAVendorFolder(t *testing.T) {
	tool, err := ResolveInstallTool("go")
	if err != nil {
		t.Skip("go is not on PATH")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod":                        "module example.org/p\n\ngo 1.20\n",
		"main.go":                       "package main\n\nimport \"example.org/dep\"\n\nfunc main() { dep.X() }\n",
		"vendor/example.org/dep/dep.go": "package dep\n\nfunc X() {}\n",
	} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	build := t.TempDir()
	for _, sub := range []string{"home", "tmp"} {
		if err := os.Mkdir(filepath.Join(build, sub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	control := exec.CommandContext(t.Context(), tool.Path, "build", "-buildvcs=false", "-o", "control", ".")
	control.Dir, control.Env = dir, goBuildEnv(build)
	if out, err := control.CombinedOutput(); err != nil {
		t.Fatalf("control: without -mod=readonly the vendored import should build: %v\n%s", err, out)
	}
	if err := RunInstallBuild(t.Context(), tool, []string{"go", "build", "-o", "plugin", "."}, dir, filepath.Join(t.TempDir(), "log")); err == nil {
		t.Fatal("the host build used the vendor folder")
	}
	if _, err := os.Stat(filepath.Join(dir, "plugin")); !os.IsNotExist(err) {
		t.Fatal("a plugin was built from vendored code", err)
	}
}

// TestRunInstallBuildTurnsGoTelemetryOff asks the real go command for its
// telemetry mode and folder inside the build environment. With "off" it
// writes no counters and starts no telemetry process, which would run in its
// own session and write into the build folder after the build.
func TestRunInstallBuildTurnsGoTelemetryOff(t *testing.T) {
	tool, err := ResolveInstallTool("go")
	if err != nil {
		t.Skip("go is not on PATH")
	}
	staging := t.TempDir()
	log := filepath.Join(staging, "log")
	if err := RunInstallBuild(t.Context(), tool, []string{"go", "env", "GOTELEMETRY", "GOTELEMETRYDIR"}, t.TempDir(), log); err != nil {
		t.Fatal(err, TailInstallLog(log, 5))
	}
	lines := strings.Split(TailInstallLog(log, 2), "\n")
	if len(lines) != 2 || lines[0] != "off" || !strings.HasPrefix(lines[1], filepath.Join(staging, ".build-")) {
		t.Fatalf("go telemetry in the build: %q", lines)
	}
	if left, _ := filepath.Glob(filepath.Join(staging, ".build-*")); len(left) != 0 {
		t.Fatal("private build folder left behind", left)
	}
}

// TestRemoveBuildFolderReportsWhatItCannotRemove keeps a file that cannot be
// deleted (its folder has no write bit) and checks that the error names the
// build folder, and that it joins with an earlier build error.
func TestRemoveBuildFolderReportsWhatItCannotRemove(t *testing.T) {
	// A user such as root may delete in a folder without the write bit. Probe
	// with a separate copy of the setup first; normal users cannot.
	probe := lockedBuildFolder(t)
	if os.RemoveAll(probe) == nil {
		t.Skip("this user can delete files in a folder without the write bit, so the cleanup error cannot be produced")
	}
	build := lockedBuildFolder(t)
	err := errors.Join(errors.New("build failed"), removeBuildFolder(build))
	if err == nil || !strings.Contains(err.Error(), "build failed") || !strings.Contains(err.Error(), "could not remove the private build folder "+strconv.Quote(build)+": ") || !errors.Is(err, os.ErrPermission) {
		t.Fatal("cleanup failure not reported with its cause", err)
	}
	if err := os.Chmod(filepath.Join(build, "home", "locked"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := removeBuildFolder(build); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(build); !os.IsNotExist(err) {
		t.Fatal("build folder still there", err)
	}
}

// lockedBuildFolder makes a build folder whose home/locked/late file sits in
// a folder without the write bit. Cleanup restores the bit.
func lockedBuildFolder(t *testing.T) string {
	t.Helper()
	build := filepath.Join(t.TempDir(), ".build-x")
	locked := filepath.Join(build, "home", "locked")
	if err := os.MkdirAll(locked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(locked, "late"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0700) })
	return build
}

// TestRunInstallBuildJoinsBuildAndCleanupErrors lets the build fail and also
// leave a folder it cannot remove: both errors must reach the owner.
func TestRunInstallBuildJoinsBuildAndCleanupErrors(t *testing.T) {
	if os.RemoveAll(lockedBuildFolder(t)) == nil {
		t.Skip("this user can delete files in a folder without the write bit, so the cleanup error cannot be produced")
	}
	tool, err := ResolveInstallTool("sh")
	if err != nil {
		t.Fatal(err)
	}
	staging := t.TempDir()
	t.Cleanup(func() {
		locked, _ := filepath.Glob(filepath.Join(staging, ".build-*", "home", "x"))
		for _, dir := range locked {
			_ = os.Chmod(dir, 0700)
		}
	})
	err = RunInstallBuild(t.Context(), tool, []string{"sh", "-c", `mkdir -p "$HOME/x/y" && chmod 500 "$HOME/x"; exit 23`}, t.TempDir(), filepath.Join(staging, "log"))
	if err == nil || !strings.Contains(err.Error(), "exit status 23") || !strings.Contains(err.Error(), "could not remove the private build folder") || !errors.Is(err, os.ErrPermission) {
		t.Fatal("build and cleanup errors not both reported", err)
	}
}

// TestRunInstallBuildLeavesTheOwnersGoTelemetryAlone sets a fake owner HOME
// with telemetry "on" and checks that the build changes nothing there.
func TestRunInstallBuildLeavesTheOwnersGoTelemetryAlone(t *testing.T) {
	tool, err := ResolveInstallTool("go")
	if err != nil {
		t.Skip("go is not on PATH")
	}
	owner := t.TempDir()
	t.Setenv("HOME", owner)
	config, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	mode := filepath.Join(config, "go", "telemetry", "mode")
	if err := os.MkdirAll(filepath.Dir(mode), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mode, []byte("on 2026-01-01"), 0600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "log")
	if err := RunInstallBuild(t.Context(), tool, []string{"go", "env", "GOTELEMETRY"}, t.TempDir(), log); err != nil || TailInstallLog(log, 1) != "off" {
		t.Fatal("build telemetry", TailInstallLog(log, 3), err)
	}
	if raw, err := os.ReadFile(mode); err != nil || string(raw) != "on 2026-01-01" {
		t.Fatalf("the owner's telemetry mode changed: %q %v", raw, err)
	}
}

func TestGoArtifactMustBeTheBuildOutput(t *testing.T) {
	ok := strings.Replace(goodManifest, `"build":["go","build","-o","plugin","."]`, `"build":["go","build","-o=plugin","."]`, 1)
	if _, err := parseInstallManifest([]byte(ok)); err != nil {
		t.Fatal("-o=NAME refused", err)
	}
	for name, raw := range map[string]string{
		"prebuilt artifact": strings.Replace(goodManifest, `"artifact":"plugin"`, `"artifact":"tools/helper"`, 1),
		"trailing space":    strings.Replace(goodManifest, `"artifact":"plugin"`, `"artifact":"plugin "`, 1),
		"no -o":             strings.Replace(goodManifest, `["go","build","-o","plugin","."]`, `["go","build","."]`, 1),
		"two -o":            strings.Replace(goodManifest, `["go","build","-o","plugin","."]`, `["go","build","-o","plugin","-o=plugin","."]`, 1),
	} {
		if _, err := parseInstallManifest([]byte(raw)); err == nil || !strings.Contains(err.Error(), "the artifact must be the one -o output of the build") {
			t.Error(name, "accepted", err)
		}
	}
}

// fakeGoTool is a go tool that only records that it ran.
func fakeGoTool(t *testing.T) (InstallTool, string) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "ran")
	fake := filepath.Join(t.TempDir(), "go")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n: > '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	sum, err := hashInstallTool(fake)
	if err != nil {
		t.Fatal(err)
	}
	return InstallTool{Path: fake, SHA256: sum}, marker
}

func goFolder(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/p\n\ngo 1.26\n"
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunInstallBuildRefusesAPrebuiltOutput(t *testing.T) {
	tool, marker := fakeGoTool(t)
	dir := goFolder(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n", "plugin": "#!/bin/sh\n"})
	err := RunInstallBuild(t.Context(), tool, []string{"go", "build", "-o", "plugin", "."}, dir, filepath.Join(t.TempDir(), "log"))
	if err == nil || !strings.Contains(err.Error(), `build: "plugin" already exists in the folder`) {
		t.Fatal("a prebuilt output was accepted", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the go tool ran", err)
	}
}

func TestRunInstallBuildRefusesFilesGoWouldAssembleOrLink(t *testing.T) {
	tool, marker := fakeGoTool(t)
	for _, name := range []string{"x.s", "x.S", "asm/y.sx", "c.c", "c.cc", "c.cpp", "c.cxx", "c.c++", "o.m", "h.h", "h.hh", "h.hpp", "h.hxx", "f.f", "f.F", "f.for", "f.f90", "w.swig", "w.swigcxx", "rsrc.syso", "UP.SYSO"} {
		dir := goFolder(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n", name: "x"})
		err := RunInstallBuild(t.Context(), tool, []string{"go", "build", "-o", "plugin", "."}, dir, filepath.Join(t.TempDir(), "log"))
		if err == nil || !strings.Contains(err.Error(), "build: "+strconv.Quote(name)+" is not allowed; go plugins may contain only Go source, go.mod and embedded data") {
			t.Error(name, "accepted", err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the go tool ran", err)
	}
	dir := goFolder(t, map[string]string{"main.go": "package main\n\nfunc main() {}\n", "page.html": "<p>", "assets/data.json": "{}", "README.md": "x"})
	if err := RunInstallBuild(t.Context(), tool, []string{"go", "build", "-o", "plugin", "."}, dir, filepath.Join(t.TempDir(), "log")); err != nil {
		t.Fatal("Go source with embedded data was refused", err)
	}
}

// TestAssemblyIncludeCannotReadOutsideTheCopy is the reported case: an
// assembly file that includes a file outside the folder. The real go tool must
// not run, and the build log must not hold the outside file's text.
func TestAssemblyIncludeCannotReadOutsideTheCopy(t *testing.T) {
	tool, err := ResolveInstallTool("go")
	if err != nil {
		t.Skip("go is not on PATH")
	}
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(outside, []byte("OUTSIDE_MARKER_TOKEN line two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := goFolder(t, map[string]string{
		"main.go": "package main\n\nfunc main() {}\n",
		"x.s":     "#include \"" + outside + "\"\n",
	})
	log := filepath.Join(t.TempDir(), "log")
	err = RunInstallBuild(t.Context(), tool, []string{"go", "build", "-o", "plugin", "."}, dir, log)
	if err == nil || !strings.Contains(err.Error(), `build: "x.s" is not allowed`) {
		t.Fatal("the assembly file was built", err)
	}
	if raw, _ := os.ReadFile(log); strings.Contains(string(raw), "OUTSIDE_MARKER_TOKEN") {
		t.Fatal("the build read a file outside the copy", string(raw))
	}
}
