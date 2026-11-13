package plugins

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// installManifestFile is the manifest name at the root of a plugin folder.
const installManifestFile = "zalki-plugin.json"

// maxInstallFiles caps files plus folders in a source; tests lower it.
var maxInstallFiles = 10000

// maxInstallBytes caps the copied source size; tests lower it.
var maxInstallBytes int64 = 200 << 20

// installTimeout stops a build that runs too long; tests shorten it.
var installTimeout = 10 * time.Minute

// InstallAccess lists the kinds of access an author asks for. A kind that is
// not declared can never be granted. Status and receive are standard access;
// send (messages nobody asked for) and read (other sessions' message bodies)
// are extended access.
type InstallAccess struct {
	Status  bool `json:"status"`
	Receive bool `json:"receive"`
	Send    bool `json:"send"`
	Read    bool `json:"read"`
}

// InstallManifest is the author's description of a local plugin folder.
// Purpose is an author claim, not an audit.
type InstallManifest struct {
	// API is the plugin wire the plugin speaks; it must be one this host speaks.
	API string `json:"api"`
	// ID names the plugin (author/name, an author claim); Name is the default
	// local instance name. One ID may be installed under many names (--as).
	ID       string        `json:"id"`
	Version  string        `json:"version"`
	Name     string        `json:"name"`
	Language string        `json:"language"`
	Purpose  string        `json:"purpose"`
	Build    []string      `json:"build"`
	Artifact string        `json:"artifact"`
	Args     []string      `json:"args"`
	Access   InstallAccess `json:"access"`
}

// InstallSource is a private snapshot of a plugin folder.
type InstallSource struct {
	Dir            string
	TreeSHA256     string
	ManifestSHA256 string
	Manifest       InstallManifest
}

// validPluginID is the one namespace rule for plugin IDs: "author/name",
// each part [a-z][a-z0-9-]{0,31}. "zalkilabs" (our plugins) and "local" (a
// folder with no published author) are the known authors. Neither is
// enforced: with no signing yet, the ID is the author's claim and the install
// screen says so. A later rule (an org name, a reserved author) goes here.
func validPluginID(id string) bool {
	author, name, ok := strings.Cut(id, "/")
	return ok && validIDPart(author) && validIDPart(name)
}

func validIDPart(part string) bool {
	if part == "" || len(part) > 32 || part[0] < 'a' || part[0] > 'z' {
		return false
	}
	for _, r := range part {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}

func installError(format string, args ...any) error {
	return fmt.Errorf("plugin install: "+format, args...)
}

func plainManifestText(value string, limit int) bool {
	return len(value) <= limit && utf8.ValidString(value) &&
		!strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsPrint(r) })
}

// parseInstallManifest decodes a manifest strictly: unknown fields, missing
// required fields and unsupported values fail before anything is shown.
func parseInstallManifest(raw []byte) (InstallManifest, error) {
	var m InstallManifest
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return m, installError("%s must be a JSON object", installManifestFile)
	}
	if _, old := fields["requests"]; old {
		return m, installError(`%s uses "requests"; replace it with "access": {"status": true, "receive": false, "send": false, "read": false}`, installManifestFile)
	}
	for _, key := range []string{"api", "id", "version", "name", "language", "purpose", "build", "artifact", "access"} {
		if _, ok := fields[key]; !ok {
			return m, installError("%s needs the %q field", installManifestFile, key)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, installError("%s: %v", installManifestFile, err)
	}
	if m.Args == nil {
		m.Args = []string{}
	}
	return m, m.validate()
}

func (m InstallManifest) validate() error {
	if err := m.validateClaims(); err != nil {
		return err
	}
	switch {
	case !plainManifestText(m.Purpose, 200):
		return installError("purpose must be plain text of at most 200 bytes")
	case !plainManifestText(m.Artifact, 256) || m.Artifact == "" || !filepath.IsLocal(m.Artifact) || filepath.Clean(m.Artifact) != m.Artifact:
		return installError("artifact must be a clean relative path inside the folder")
	case !m.Access.Status:
		return installError("access.status must be true: an installed plugin needs at least one session")
	}
	if err := m.validateBuild(); err != nil {
		return err
	}
	for _, list := range [][]string{m.Build, m.Args} {
		size := 0
		for _, arg := range list {
			size += len(arg)
			if !plainManifestText(arg, 1024) {
				return installError("build and args must be plain text")
			}
		}
		if len(list) > 8 || size > 1024 {
			return installError("build and args allow at most 8 items and 1024 bytes")
		}
	}
	return nil
}

// validateClaims checks the wire, the plugin ID and the version.
func (m InstallManifest) validateClaims() error {
	switch {
	case m.API != wireName:
		return installError("api %q is not a plugin wire this Zalki speaks; it speaks %q. Rebuild the plugin for it, or update Zalki", m.API, wireName)
	case !validPluginID(m.ID):
		return installError(`id must be "author/name", each part a lowercase letter then up to 31 lowercase letters, digits or '-' (for example "local/%s")`, m.Name)
	case m.Version == "" || !plainManifestText(m.Version, 32):
		return installError("version must be plain text of 1 to 32 bytes")
	}
	return nil
}

func (m InstallManifest) validateBuild() error {
	switch m.Language {
	case "go":
		if err := validateGoBuild(m.Build); err != nil {
			return err
		}
		if name, n := goBuildOutput(m.Build); n != 1 || name != m.Artifact {
			return installError("build: the artifact must be the one -o output of the build (%s)", strconv.Quote(m.Artifact))
		}
	case "native":
		if m.Build != nil {
			return installError(`language "native" needs "build": null`)
		}
	default:
		return installError(`language must be "go" or "native"`)
	}
	return nil
}

// goBuildFlags are the only single build arguments allowed after "go build".
var goBuildFlags = []string{"-trimpath", "-mod=readonly", "-p=1", "-p=2", "-ldflags=-s -w"}

// goBuildPairs are the flags allowed with their value in the next argument.
var goBuildPairs = map[string]func(string) bool{
	"-o":       plainBuildOutput,
	"-ldflags": func(v string) bool { return v == "-s -w" },
}

// validateGoBuild allows only "go build" with the flags above and packages
// inside the copy. A flag such as -gcflags, -toolexec, -C or -overlay could
// write, read or run something outside the reviewed copy, so every other
// argument is refused. The error names the first refused argument.
func validateGoBuild(argv []string) error {
	if len(argv) < 2 || argv[0] != "go" || argv[1] != "build" {
		return installError(`language "go" needs a build command that starts with "go build"`)
	}
	for i := 2; i < len(argv); i++ {
		arg := argv[i]
		valid, ok := goBuildPairs[arg]
		switch {
		case slices.Contains(goBuildFlags, arg), localGoPackage(arg):
		case strings.HasPrefix(arg, "-o=") && plainBuildOutput(strings.TrimPrefix(arg, "-o=")):
		case ok && i+1 < len(argv) && valid(argv[i+1]):
			i++
		default:
			return installError("build: %s is not allowed; use only -trimpath, -mod=readonly, -p=1 or -p=2, -o NAME, -ldflags \"-s -w\" and . or ./path", strconv.Quote(arg))
		}
	}
	return nil
}

// goBuildOutput returns the -o name of a go build argv and how many -o flags
// it has.
func goBuildOutput(argv []string) (name string, count int) {
	for i := 2; i < len(argv); i++ {
		switch {
		case argv[i] == "-o" && i+1 < len(argv):
			name, count = argv[i+1], count+1
			i++
		case strings.HasPrefix(argv[i], "-o="):
			name, count = strings.TrimPrefix(argv[i], "-o="), count+1
		}
	}
	return name, count
}

// plainBuildOutput accepts one plain file name in the copy, with no folder.
func plainBuildOutput(name string) bool {
	return filepath.IsLocal(name) && !strings.ContainsAny(name, `/\`) && !strings.HasPrefix(name, "-")
}

// localGoPackage accepts "." and "./dir/sub" inside the copy. Import paths,
// file arguments, "..." patterns and versions are refused: they can name code
// outside the reviewed copy.
func localGoPackage(arg string) bool {
	rel, ok := strings.CutPrefix(arg, "./")
	return arg == "." || ok && filepath.IsLocal(rel) && filepath.Clean(rel) == rel &&
		!strings.Contains(rel, "..") && !strings.ContainsAny(rel, "@\\") && !strings.HasPrefix(rel, "-") && !strings.HasSuffix(rel, ".go")
}

// goBuildArgs adds two flags to a go build: -buildvcs=false, so the build
// never runs git on the copy, and -mod=readonly, so a vendor folder is never
// used and the build stays with the standard library.
func goBuildArgs(argv []string) []string {
	if len(argv) > 1 && argv[0] == "go" && argv[1] == "build" {
		return slices.Concat([]string{"build", "-buildvcs=false", "-mod=readonly"}, argv[2:])
	}
	return argv[1:]
}

// goBuildEnv is the whole build environment. Nothing else from the owner's
// environment is kept: no toolchain switch, no "go env -w" file, no GOFLAGS,
// no workspace, no helper commands (GOCACHEPROG, GOAUTH), no cgo, no network
// and at most two CPUs. Caches live in the private build folder.
func goBuildEnv(build string) []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(build, "home"),
		"TMPDIR=" + filepath.Join(build, "tmp"),
		"GOCACHE=" + filepath.Join(build, "cache"),
		"GOMODCACHE=" + filepath.Join(build, "mod"),
		"GOPATH=" + filepath.Join(build, "gopath"),
		"GOTOOLCHAIN=local", "GOENV=off", "GOFLAGS=", "GOWORK=off", "CGO_ENABLED=0", "GOPROXY=off", "GOSUMDB=off", "GOMAXPROCS=2",
	}
}

// goTelemetryOff writes Go's telemetry mode file "off" in the private HOME
// (the os.UserConfigDir of that HOME). Without it the go command writes
// counter files and starts a telemetry process in its own session, which
// outlives the build and writes into the build folder after it is removed.
func goTelemetryOff(home string) error {
	config := filepath.Join(home, ".config")
	if runtime.GOOS == "darwin" {
		config = filepath.Join(home, "Library", "Application Support")
	}
	dir := filepath.Join(config, "go", "telemetry")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "mode"), []byte("off\n"), 0600)
}

// removeBuildFolder removes the private build folder. It tries twice, in
// case a late writer adds a file while RemoveAll runs, and then names the
// folder instead of leaving files behind without a word.
func removeBuildFolder(build string) error {
	var cause error
	for range 2 {
		if cause = os.RemoveAll(build); cause == nil {
			_, cause = os.Lstat(build)
			if errors.Is(cause, os.ErrNotExist) {
				return nil
			}
			cause = cmp.Or(cause, errors.New("a file was added during the removal"))
		}
	}
	return installError("could not remove the private build folder %s: %w", strconv.Quote(build), cause)
}

// ReadInstallManifest reads and checks the manifest from a folder without following a link.
func ReadInstallManifest(dir string) (InstallManifest, error) {
	raw, err := readInstallManifest(dir)
	if err != nil {
		return InstallManifest{}, err
	}
	return parseInstallManifest(raw)
}

func readInstallManifest(dir string) ([]byte, error) { return readInstallManifestObserved(dir, nil) }

func readInstallManifestObserved(dir string, observe func(*os.File)) ([]byte, error) {
	fd, err := syscall.Open(filepath.Join(dir, installManifestFile), syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, installError("read %s: %w", installManifestFile, err)
	}
	file := os.NewFile(uintptr(fd), installManifestFile)
	if observe != nil {
		observe(file)
	}
	defer file.Close() //nolint:errcheck // Read-only manifest.
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, installError("%s must be a regular file", installManifestFile)
	}
	raw, err := io.ReadAll(io.LimitReader(file, 16<<10+1))
	if err == nil && len(raw) > 16<<10 {
		err = installError("%s is larger than 16 KiB", installManifestFile)
	}
	return raw, err
}

// ResolveInstallSource returns the real folder path. Only folders are accepted.
func ResolveInstallSource(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err == nil {
		abs, err = filepath.EvalSymlinks(abs)
	}
	if err != nil {
		return "", installError("source folder: %w", err)
	}
	if info, err := os.Lstat(abs); err != nil || !info.IsDir() {
		return "", installError("source must be a folder")
	}
	return abs, nil
}

// vcsFolders are not copied, at any depth.
var vcsFolders = []string{".git", ".hg", ".svn", ".bzr"}

type snapshot struct {
	target string
	files  int
	bytes  int64
	digest io.Writer
}

// beforeSnapshotOpen runs after an entry is checked and before it is opened;
// tests use it to swap the entry.
var beforeSnapshotOpen = func(string) {}

// SnapshotInstallSource copies a folder into an empty private staging folder
// and hashes the copy. Links and devices found in the folder are refused, and
// nothing in the folder runs. What is guaranteed during a concurrent change:
// every open is relative to an already open parent folder (os.Root), so
// nothing outside the source folder is read; and each opened entry must be the
// same file (device and inode) that was checked, unless it was deleted and
// recreated with a reused inode, or a link swapped in leads back to the same
// folder. This is a consistency guard, not the security boundary: the digest
// shown for review is the hash of the COPY, and the build runs on the copy, so
// a change in the source during the walk cannot make the owner approve one
// thing and run another. The manifest is read back from the copy.
func SnapshotInstallSource(source, staging string) (InstallSource, error) {
	result := InstallSource{Dir: filepath.Join(staging, "src")}
	root, err := os.OpenRoot(source)
	if err != nil {
		return result, err
	}
	defer root.Close() //nolint:errcheck // Read-only source walk.
	hash := sha256.New()
	s := snapshot{target: result.Dir, digest: hash}
	if err := s.copyDir(root, "."); err != nil {
		return result, err
	}
	result.TreeSHA256 = hex.EncodeToString(hash.Sum(nil))
	raw, err := readInstallManifest(result.Dir)
	if err != nil {
		return result, err
	}
	sum := sha256.Sum256(raw)
	result.ManifestSHA256 = hex.EncodeToString(sum[:])
	result.Manifest, err = parseInstallManifest(raw)
	return result, err
}

// goModDirectives are the only go.mod directives a go plugin may use. Any
// require, replace, exclude, retract, tool or ignore could bring in code that
// is not in the reviewed copy, even with no network.
var goModDirectives = []string{"module", "go", "toolchain", "godebug"}

// goToolFileTypes are file types that go build compiles, assembles or links
// besides Go source. An assembly file can #include a file outside the copy,
// and a .syso object is linked in as it is, so none is allowed.
var goToolFileTypes = []string{".s", ".sx", ".c", ".cc", ".cpp", ".cxx", ".c++", ".m", ".h", ".hh", ".hpp", ".hxx", ".f", ".for", ".f90", ".swig", ".swigcxx", ".syso"}

// checkGoSourceFiles refuses the first such file in the copy. The match
// ignores case, so .S and .F are refused too.
func checkGoSourceFiles(dir string) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		if slices.Contains(goToolFileTypes, strings.ToLower(filepath.Ext(path))) {
			rel, _ := filepath.Rel(dir, path)
			return installError("build: %s is not allowed; go plugins may contain only Go source, go.mod and embedded data", strconv.Quote(rel))
		}
		return nil
	})
}

// checkBuildOutputFree refuses a copy that already holds the -o file, so the
// pinned artifact is always the one the build wrote.
func checkBuildOutputFree(dir string, argv []string) error {
	name, _ := goBuildOutput(argv)
	if name == "" {
		return nil // the manifest check already requires exactly one -o
	}
	if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
		return installError("build: %s already exists in the folder; the artifact must come from the build", strconv.Quote(name))
	}
	return nil
}

// checkGoModule requires a go.mod at the root of the copy, so Go never uses
// one from a parent folder, and allows only standard-library builds.
func checkGoModule(dir string) error {
	path := filepath.Join(dir, "go.mod")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return installError("plugins must use only the Go standard library: the folder needs a go.mod file of at most 64 KiB at its root")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the go.mod of our own staged copy.
	if err != nil {
		return err
	}
	inBlock := false
	for _, line := range strings.Split(string(raw), "\n") {
		line, _, _ = strings.Cut(line, "//")
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case inBlock:
			inBlock = line != ")"
		default:
			directive := line[:len(line)-len(strings.TrimLeftFunc(line, unicode.IsLetter))]
			if !slices.Contains(goModDirectives, directive) {
				return installError("plugins must use only the Go standard library: go.mod has a %s directive", strconv.Quote(cmp.Or(directive, line)))
			}
			rest := strings.TrimSpace(line[len(directive):])
			inBlock = strings.HasPrefix(rest, "(") && !strings.HasSuffix(rest, ")")
		}
	}
	return nil
}

// copyDir records and creates one folder, then copies its entries in name
// order (the same order as filepath.WalkDir).
func (s *snapshot) copyDir(dir *os.Root, rel string) error {
	s.files++ // readNames below refuses the folder once the count passes the cap
	if _, err := fmt.Fprintf(s.digest, "d - %s\n", strconv.Quote(rel)); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(s.target, rel), 0700); err != nil {
		return err
	}
	names, err := s.readNames(dir)
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		if slices.Contains(vcsFolders, name) {
			continue // VCS metadata can name helper commands; the build does not need it
		}
		if err := s.copyEntry(dir, name, filepath.Join(rel, name)); err != nil {
			return err
		}
	}
	return nil
}

// readInstallNames reads one chunk of folder names; tests count the calls.
var readInstallNames = func(list *os.File) ([]string, error) { return list.Readdirnames(256) }

// readNames reads a folder in chunks and stops as soon as the names would pass
// the file cap, so a huge folder is never read whole.
func (s *snapshot) readNames(dir *os.Root) ([]string, error) {
	list, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer list.Close() //nolint:errcheck // Read-only folder listing.
	var names []string
	for {
		chunk, err := readInstallNames(list)
		names = append(names, chunk...)
		if s.files+len(names) > maxInstallFiles {
			return nil, installError("source has more than %d files and folders", maxInstallFiles)
		}
		if errors.Is(err, io.EOF) || (err == nil && len(chunk) == 0) {
			return names, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

func (s *snapshot) copyEntry(dir *os.Root, name, rel string) error {
	info, err := dir.Lstat(name)
	if err != nil {
		return err
	}
	switch {
	case info.IsDir():
		beforeSnapshotOpen(rel)
		child, err := dir.OpenRoot(name)
		if err != nil {
			return errors.Join(err, installError("%s changed while copying", strconv.Quote(rel)))
		}
		defer child.Close() //nolint:errcheck // Read-only source walk.
		if opened, err := child.Stat("."); err != nil || !os.SameFile(info, opened) {
			return errors.Join(err, installError("%s changed while copying", strconv.Quote(rel)))
		}
		return s.copyDir(child, rel)
	case info.Mode().IsRegular():
		if s.files++; s.files > maxInstallFiles {
			return installError("source has more than %d files and folders", maxInstallFiles)
		}
		beforeSnapshotOpen(rel)
		return s.copyFile(dir, name, info, rel)
	default:
		return installError("%s is not a regular file or folder; links and devices are not allowed", strconv.Quote(rel))
	}
}

func (s *snapshot) copyFile(dir *os.Root, name string, checked fs.FileInfo, rel string) error {
	// os.Root follows a link even with O_NOFOLLOW, so the SameFile check below
	// is what refuses a swapped entry. O_NONBLOCK keeps a swapped FIFO from blocking.
	input, err := dir.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return errors.Join(err, installError("%s changed while copying", strconv.Quote(rel)))
	}
	defer input.Close() //nolint:errcheck // Read-only source copy.
	info, err := input.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(checked, info) {
		return errors.Join(err, installError("%s changed while copying", strconv.Quote(rel)))
	}
	kind, mode := "f", os.FileMode(0600)
	if info.Mode().Perm()&0111 != 0 {
		kind, mode = "x", 0700
	}
	output, err := os.OpenFile(filepath.Join(s.target, rel), os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) //nolint:gosec // G304: a new file inside our private staging copy.
	if err != nil {
		return err
	}
	hash := sha256.New()
	limit := maxInstallBytes - s.bytes
	n, err := io.Copy(io.MultiWriter(output, hash), io.LimitReader(input, limit+1))
	err = errors.Join(err, output.Close())
	if s.bytes += n; err == nil && s.bytes > maxInstallBytes {
		err = installError("source is larger than %d MiB", maxInstallBytes>>20)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(s.digest, "%s %x %s\n", kind, hash.Sum(nil), strconv.Quote(rel))
	return err
}

// InstallTool is the real file of the build tool (links resolved) and its hash.
// The build runs this path, not the PATH entry, after checking the hash again.
type InstallTool struct {
	Path   string
	SHA256 string
}

// ResolveInstallTool finds the build tool on PATH. LookPath refuses a result
// from a relative PATH entry (exec.ErrDot), so the path is absolute.
func ResolveInstallTool(name string) (InstallTool, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return InstallTool{}, errors.Join(err, installError("build tool %q was not found on PATH", name))
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return InstallTool{}, err
	}
	sum, err := hashInstallTool(resolved)
	return InstallTool{Path: resolved, SHA256: sum}, err
}

func hashInstallTool(path string) (string, error) {
	raw, err := os.Open(path) //nolint:gosec // G304: the resolved build tool the owner reviewed.
	if err != nil {
		return "", err
	}
	defer raw.Close() //nolint:errcheck // Read-only tool hash.
	hash := sha256.New()
	if _, err := io.Copy(hash, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// maxInstallLog caps the build log; a build that writes more is stopped.
var maxInstallLog int64 = 16 << 20

// installWaitDelay bounds the wait for output pipes held after the leader exits.
var installWaitDelay = 5 * time.Second

// installLog writes build output to the log until the cap, then stops the
// build once and drops the rest, so the output pipe never blocks.
type installLog struct {
	mu   sync.Mutex
	file *os.File
	left int64
	over bool
	stop func()
}

// Write records build output up to the cap and stops the build once it is
// exceeded. Further output is consumed and discarded so the pipe cannot block.
func (l *installLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.over {
		return len(p), nil
	}
	if int64(len(p)) > l.left {
		l.over = true
		_, err := l.file.Write(p[:l.left])
		l.stop()
		return len(p), err
	}
	l.left -= int64(len(p))
	return l.file.Write(p)
}

// RunInstallBuild runs the literal build argv in the staged copy without a
// shell, with goBuildEnv and caches in a private folder next to log that is
// removed afterwards. It has no sandbox. Output goes to log.
func RunInstallBuild(ctx context.Context, tool InstallTool, argv []string, dir, log string) (err error) {
	output, err := os.OpenFile(log, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // G304: our own staging log.
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	// Check the reviewed bytes again right before running them.
	if sum, err := hashInstallTool(tool.Path); err != nil || sum != tool.SHA256 {
		return errors.Join(err, installError("build tool %s changed after the review; nothing was built", strconv.Quote(tool.Path)), output.Close())
	}
	build, err := os.MkdirTemp(filepath.Dir(log), ".build-")
	if err != nil {
		return errors.Join(err, output.Close())
	}
	defer func() { err = errors.Join(err, removeBuildFolder(build)) }()
	err = errors.Join(os.Mkdir(filepath.Join(build, "home"), 0700), os.Mkdir(filepath.Join(build, "tmp"), 0700), goTelemetryOff(filepath.Join(build, "home")))
	if err != nil {
		return errors.Join(err, output.Close())
	}
	if len(argv) > 1 && argv[0] == "go" && argv[1] == "build" {
		if err := errors.Join(checkGoModule(dir), checkGoSourceFiles(dir), checkBuildOutputFree(dir, argv)); err != nil {
			return errors.Join(err, output.Close())
		}
	}
	capped := &installLog{file: output, left: maxInstallLog, stop: cancel}
	cmd := exec.CommandContext(ctx, tool.Path, goBuildArgs(argv)...) //nolint:gosec // G204: the owner approved this literal argv.
	cmd.Dir, cmd.Stdout, cmd.Stderr, cmd.Env = dir, capped, capped, goBuildEnv(build)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// Wait stops waiting for held output pipes this long after the leader exits.
	cmd.WaitDelay = installWaitDelay
	if err = cmd.Start(); err != nil {
		return errors.Join(err, output.Close())
	}
	err = cmd.Wait()
	// Go stops watching the group when the leader exits, so a background child
	// could keep running and writing. Always end the whole group here.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	capped.mu.Lock()
	over := capped.over
	capped.mu.Unlock()
	switch {
	case over:
		err = errors.Join(err, installError("build output passed %d MiB, so the build was stopped", maxInstallLog>>20))
	case ctx.Err() != nil:
		err = errors.Join(err, installError("build stopped after %s", installTimeout))
	case errors.Is(err, exec.ErrWaitDelay):
		err = installError("the build left a background process holding its output; it was stopped")
	}
	capped.mu.Lock()
	defer capped.mu.Unlock()
	capped.over = true // drop anything a stopped child still had in flight
	return errors.Join(err, output.Close())
}

// StagedInstallArtifact returns the built artifact path after checking that no
// part of its path inside the staged copy is a link.
func StagedInstallArtifact(dir, rel string) (string, error) {
	path := dir
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if err != nil {
			return "", installError("artifact %s was not built: %w", strconv.Quote(rel), err)
		}
		if last := i == len(parts)-1; (!last && !info.IsDir()) || (last && (!info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0)) {
			return "", installError("artifact %s must be an executable regular file with no link in its path", strconv.Quote(rel))
		}
	}
	return path, nil
}

// TailInstallLog returns up to the last n lines of the final 64 KiB of a log.
func TailInstallLog(log string, n int) string {
	file, err := os.Open(log) //nolint:gosec // G304: our own staging log.
	if err != nil {
		return ""
	}
	defer file.Close() //nolint:errcheck // Read-only log.
	if info, err := file.Stat(); err == nil && info.Size() > 64<<10 {
		_, _ = file.Seek(-64<<10, io.SeekEnd)
	}
	raw, _ := io.ReadAll(io.LimitReader(file, 64<<10))
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
