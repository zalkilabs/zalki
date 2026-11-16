package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

// The login item is one fixed launchd job per account: it runs the plugin
// supervisor for the default state at every login.
const (
	loginItemLabel = "dev.zalki.plugins"
	loginItemLimit = 16 << 10
)

// Managed states of the plist file, as inspect reports them.
const (
	itemMissing       = "missing"
	itemManaged       = "managed"
	itemChanged       = "changed"
	itemForeign       = "foreign"
	itemUnsafe        = "unsafe"
	itemInvalidRecord = "invalid_record"
	itemUnknown       = "unknown"
)

// loginItemPaths are the fixed paths for one account home and state.
type loginItemPaths struct {
	home, state, plist, record, pending, lock string
}

func newLoginItemPaths(home, state string) loginItemPaths {
	plugins := filepath.Join(state, "plugins")
	return loginItemPaths{
		home:    home,
		state:   state,
		plist:   filepath.Join(home, "Library", "LaunchAgents", loginItemLabel+".plist"),
		record:  filepath.Join(plugins, "login-item.installation.json"),
		pending: filepath.Join(plugins, "login-item.installation.pending.json"),
		lock:    filepath.Join(plugins, ".login-item.operation.lock"),
	}
}

// renderLoginItem is the exact plist for the stable launcher binary and the
// canonical state: fixed keys in a fixed order, values XML-escaped, a final
// LF; no environment, log paths or other keys.
func renderLoginItem(binary, state string) []byte {
	var b bytes.Buffer
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	b.WriteString("<plist version=\"1.0\">\n<dict>\n  <key>Label</key>\n  <string>" + loginItemLabel + "</string>\n")
	b.WriteString("  <key>ProgramArguments</key>\n  <array>\n")
	for _, arg := range []string{binary, "plugins", "supervise", "--state", state} {
		b.WriteString("    <string>")
		_ = xml.EscapeText(&b, []byte(arg))
		b.WriteString("</string>\n")
	}
	b.WriteString("  </array>\n  <key>RunAtLoad</key>\n  <true/>\n")
	b.WriteString("  <key>KeepAlive</key>\n  <dict>\n    <key>SuccessfulExit</key>\n    <false/>\n  </dict>\n</dict>\n</plist>\n")
	return b.Bytes()
}

func sha256Hex(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// loginItemRecord is the committed ownership of the plist: whose account,
// which state, which launcher, and the exact bytes Zalki wrote.
type loginItemRecord struct {
	Schema     int    `json:"schema"`
	UID        int    `json:"uid"`
	Home       string `json:"home"`
	State      string `json:"state"`
	Label      string `json:"label"`
	Path       string `json:"path"`
	Binary     string `json:"binary"`
	PlistSHA   string `json:"plist_sha256"`
	Generation uint64 `json:"generation"`
}

// loginItemPending is written and synced BEFORE any plist or launchd
// change and removed only after the change is committed: it lets a later
// operation (holding the lock) see an unfinished one.
type loginItemPending struct {
	Schema    int              `json:"schema"`
	Operation string           `json:"operation"`
	Action    string           `json:"action"`
	Phase     string           `json:"phase"`
	Prior     *loginItemRecord `json:"prior"`
	Desired   *loginItemRecord `json:"desired"`
}

func (r loginItemRecord) valid(p loginItemPaths) bool {
	return r.Schema == 1 && r.UID == os.Getuid() && r.Home == p.home && r.State == p.state && r.Label == loginItemLabel &&
		r.Path == p.plist && filepath.IsAbs(r.Binary) && r.PlistSHA == sha256Hex(renderLoginItem(r.Binary, r.State)) && r.Generation > 0
}

// valid checks the whole unfinished operation: its token, action and
// phase, and the identities it may recover to. An install has the desired
// record (and the one it replaces, if any), a start the owned record as
// both, an uninstall only the prior one.
func (o loginItemPending) valid(p loginItemPaths) bool {
	if o.Schema != 1 || (o.Phase != "prepared" && o.Phase != "recovering") || len(o.Operation) != 32 || strings.Trim(o.Operation, "0123456789abcdef") != "" {
		return false
	}
	switch o.Action {
	case "install":
		return o.validInstall(p)
	case "start":
		return o.Prior != nil && o.Desired != nil && o.Prior.valid(p) && *o.Prior == *o.Desired
	case "uninstall":
		return o.Desired == nil && o.Prior != nil && o.Prior.valid(p)
	case "relaunch":
		return validRelaunch(o, p)
	}
	return false
}

// validRelaunch: the same item moved to another launcher, one generation up.
func validRelaunch(o loginItemPending, p loginItemPaths) bool {
	return o.Prior != nil && o.Desired != nil && o.Prior.valid(p) && o.Desired.valid(p) &&
		o.Desired.Generation == o.Prior.Generation+1 && o.Desired.Binary != o.Prior.Binary
}

// validInstall: a first setup (generation 1) or one over the record it
// replaces (one generation above it).
func (o loginItemPending) validInstall(p loginItemPaths) bool {
	if o.Desired == nil || !o.Desired.valid(p) {
		return false
	}
	if o.Prior == nil {
		return o.Desired.Generation == 1
	}
	return o.Prior.valid(p) && o.Desired.Generation == o.Prior.Generation+1
}

// readPrivateJSON strictly decodes a private regular file of this user;
// ok is false when it is missing.
func readPrivateJSON(path string, into any) (bool, error) {
	raw, err := readPrivateFile(path, loginItemLimit)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return false, err
	}
	if decoder.More() {
		return false, errors.New("trailing data")
	}
	// Only the exact bytes writePrivateJSON produces: a duplicate key, a
	// null, another order or a hand edit is not a record Zalki wrote.
	if canonical, err := json.Marshal(into); err != nil || !bytes.Equal(canonical, raw) {
		return false, errors.New("not a canonical record")
	}
	return true, nil
}

// safeChain checks every folder from the account home down to dir: each
// must be a real directory (never a link) of this user that others cannot
// write. With create, a missing folder is made (0700) one level at a time,
// so nothing is ever created through a link.
func safeChain(home, dir string, create bool) error {
	rel, err := filepath.Rel(home, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
		return errors.New(dir + " is outside the account home; nothing was changed")
	}
	current := home
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && create {
			if err := os.Mkdir(current, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return err
			}
			info, err = os.Lstat(current)
		}
		if errors.Is(err, os.ErrNotExist) {
			return nil // nothing below a missing folder exists
		}
		if err != nil {
			return err
		}
		if !privateDir(info) {
			return errors.New(current + " is not a private directory of this user; nothing was changed")
		}
	}
	return nil
}

func privateDir(info os.FileInfo) bool {
	uid, ok := fileUID(info)
	return ok && info.IsDir() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o022 == 0 && uid == currentUID()
}

// readPrivateFile reads path without following a link, refusing anything
// but a bounded regular file this user owns that others cannot write.
func readPrivateFile(path string, limit int64) ([]byte, error) {
	file, err := openNoFollow(path)
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // Read-only.
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	uid, ok := fileUID(info)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || uid != currentUID() || info.Size() > limit {
		return nil, errUnsafeLoginFile
	}
	return io.ReadAll(io.LimitReader(file, limit+1))
}

var errUnsafeLoginFile = errors.New("not a private regular file of this user")

// writePrivateJSON publishes a record atomically and durably (0600).
func writePrivateJSON(path string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, raw)
}

// loginItemFiles is what is on disk now: the plist bytes, the committed
// record and an unfinished operation, each strictly read.
type loginItemFiles struct {
	plist      []byte
	plistErr   error
	record     *loginItemRecord
	pending    *loginItemPending
	recordErr  error
	pendingErr error
}

func readLoginItemFiles(p loginItemPaths) loginItemFiles {
	var f loginItemFiles
	// The folder chain is checked BEFORE the file is opened, so a rejected
	// ancestor is never followed, not even to read.
	if err := safeChain(p.home, filepath.Dir(p.plist), false); err != nil {
		f.plistErr = errors.Join(errUnsafeLoginFile, err)
	} else {
		f.plist, f.plistErr = readPrivateFile(p.plist, loginItemLimit)
	}
	if err := safeChain(p.home, filepath.Dir(p.record), false); err != nil {
		f.recordErr = errors.Join(errInvalidLoginRecord, err)
		return f
	}
	var record loginItemRecord
	if ok, err := readPrivateJSON(p.record, &record); err != nil || (ok && !record.valid(p)) {
		f.recordErr = errors.Join(errInvalidLoginRecord, err)
	} else if ok {
		f.record = &record
	}
	var pending loginItemPending
	if ok, err := readPrivateJSON(p.pending, &pending); err != nil || (ok && !pending.valid(p)) {
		f.pendingErr = errors.Join(errInvalidLoginRecord, err)
	} else if ok {
		f.pending = &pending
	}
	return f
}

var errInvalidLoginRecord = errors.New("the login item's ownership record is damaged")

// state classifies the plist file. Byte-identical text without a record is
// still foreign: Zalki never adopts a file it did not write.
func (f loginItemFiles) state() string {
	switch {
	case f.recordErr != nil || f.pendingErr != nil:
		return itemInvalidRecord
	case errors.Is(f.plistErr, os.ErrNotExist):
		return itemMissing
	case errors.Is(f.plistErr, errUnsafeLoginFile) || isLinkLoop(f.plistErr):
		return itemUnsafe
	case f.plistErr != nil:
		return itemUnknown
	case f.record == nil && f.pending == nil:
		return itemForeign
	case !f.owns(sha256Hex(f.plist)):
		if f.record == nil {
			return itemForeign
		}
		return itemChanged
	}
	return itemManaged
}

// owns reports bytes Zalki can prove it wrote: the committed record's, or
// the prior or desired bytes of its own unfinished operation.
func (f loginItemFiles) owns(sha string) bool {
	if f.record != nil && f.record.PlistSHA == sha {
		return true
	}
	if f.pending == nil {
		return false
	}
	for _, r := range []*loginItemRecord{f.pending.Prior, f.pending.Desired} {
		if r != nil && r.PlistSHA == sha {
			return true
		}
	}
	return false
}
