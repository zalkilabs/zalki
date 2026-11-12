package plugins

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
