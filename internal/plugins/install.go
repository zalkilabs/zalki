package plugins

import (
	"bytes"
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

// InstallRequests lists the kinds of access an author asks for. The owner
// picks sessions; a kind that is not requested can never be granted.
type InstallRequests struct {
	Feed       bool `json:"feed"`
	Read       bool `json:"read"`
	Send       bool `json:"send"`
	InboxRead  bool `json:"inbox_read"`
	AcceptFrom bool `json:"accept_from"`
}

// InstallManifest is the author's description of a local plugin folder.
// Purpose is an author claim, not an audit.
type InstallManifest struct {
	Name     string          `json:"name"`
	Language string          `json:"language"`
	Purpose  string          `json:"purpose"`
	Build    []string        `json:"build"`
	Artifact string          `json:"artifact"`
	Args     []string        `json:"args"`
	Requests InstallRequests `json:"requests"`
}

// InstallSource is a private snapshot of a plugin folder.
type InstallSource struct {
	Dir            string
	TreeSHA256     string
	ManifestSHA256 string
	Manifest       InstallManifest
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
	for _, key := range []string{"name", "language", "purpose", "build", "artifact", "requests"} {
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
	switch {
	case !plainManifestText(m.Purpose, 200):
		return installError("purpose must be plain text of at most 200 bytes")
	case !plainManifestText(m.Artifact, 256) || m.Artifact == "" || !filepath.IsLocal(m.Artifact) || filepath.Clean(m.Artifact) != m.Artifact:
		return installError("artifact must be a clean relative path inside the folder")
	case !m.Requests.Feed:
		return installError("requests.feed must be true: an installed plugin needs at least one session")
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

func (m InstallManifest) validateBuild() error {
	switch m.Language {
	case "go":
		if len(m.Build) == 0 || m.Build[0] != "go" {
			return installError(`language "go" needs a build command that starts with "go"`)
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

// ReadInstallManifest reads and checks the manifest from a folder without following a link.
func ReadInstallManifest(dir string) (InstallManifest, error) {
	raw, err := readInstallManifest(dir)
	if err != nil {
		return InstallManifest{}, err
	}
	return parseInstallManifest(raw)
}

func readInstallManifest(dir string) ([]byte, error) {
	fd, err := syscall.Open(filepath.Join(dir, installManifestFile), syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, installError("read %s: %w", installManifestFile, err)
	}
	syscall.CloseOnExec(fd)
	file := os.NewFile(uintptr(fd), installManifestFile)
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
// shell. It uses the owner's environment and has no sandbox. Output goes to log.
func RunInstallBuild(ctx context.Context, tool InstallTool, argv []string, dir, log string) error {
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
	capped := &installLog{file: output, left: maxInstallLog, stop: cancel}
	cmd := exec.CommandContext(ctx, tool.Path, argv[1:]...) //nolint:gosec // G204: the owner approved this literal argv.
	cmd.Dir, cmd.Stdout, cmd.Stderr = dir, capped, capped
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
