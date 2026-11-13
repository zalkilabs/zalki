package pluginfeed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// PluginInstallReceipt records what plugins install built and pinned.
// It is evidence about inputs, not publisher proof or a sandbox claim.
type PluginInstallReceipt struct {
	// API, ID and Version are the manifest's claims, kept for list and show.
	API            string   `json:"api"`
	ID             string   `json:"id"`
	Version        string   `json:"version"`
	Source         string   `json:"source"`
	TreeSHA256     string   `json:"tree_sha256"`
	ManifestSHA256 string   `json:"manifest_sha256"`
	Build          []string `json:"build"`
	Tool           string   `json:"tool,omitempty"`
	ToolSHA256     string   `json:"tool_sha256,omitempty"`
	Artifact       string   `json:"artifact"`
	ArtifactSHA256 string   `json:"artifact_sha256"`
	InstalledAt    string   `json:"installed_at"`
	// Access is the reviewed manifest's declared access: exactly the four
	// booleans status, receive, send and read. The grants live in the record.
	Access json.RawMessage `json:"access"`
	// InstalledBy is who installed it, proven at commit: a native session ID,
	// "terminal" (an anonymous, reviewed terminal) or "unproven".
	InstalledBy string `json:"installed_by"`
	// InstalledByLabel is that session's display name at install time: a
	// historical claim, never proof or a lookup key.
	InstalledByLabel string `json:"installed_by_label,omitempty"`
}

// PluginInstall holds the name lease for one install attempt and owns its
// staging folder. Close always releases the lease and removes staging unless
// a build log was kept for the owner.
type PluginInstall struct {
	feed    PluginFeed
	lease   *os.File
	keepLog bool
	update  []byte // exact record reviewed by an update; nil for a new install
}

// Tests change the artifact copy before it is pinned, or after pinning.
var beforePluginInstallPin, beforePluginInstallCommit = func() {}, func() {}

func printableText(value string, limit int) bool {
	if len(value) > limit || !utf8.ValidString(value) {
		return false
	}
	return !strings.ContainsFunc(value, func(r rune) bool { return !unicode.IsPrint(r) })
}

func (r *PluginInstallReceipt) valid() bool {
	if !validPluginSHA(r.TreeSHA256) || !validPluginSHA(r.ManifestSHA256) || !validPluginSHA(r.ArtifactSHA256) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, r.InstalledAt)
	_, declared := decodeDeclaredAccess(r.Access)
	return err == nil && printableText(r.InstalledAt, 64) && r.validPaths() && r.validBuild() && r.validClaims() && declared && validProvenance(r.InstalledBy, r.InstalledByLabel)
}

func (r *PluginInstallReceipt) validPaths() bool {
	return printableText(r.Source, 1024) && filepath.IsAbs(r.Source) && filepath.Clean(r.Source) == r.Source &&
		printableText(r.Artifact, 256) && r.Artifact != "" && filepath.IsLocal(r.Artifact) && filepath.Clean(r.Artifact) == r.Artifact
}

// validClaims bounds the manifest claims; the manifest check is in plugins.
func (r *PluginInstallReceipt) validClaims() bool {
	return r.API != "" && printableText(r.API, 32) && r.ID != "" && printableText(r.ID, 65) && r.Version != "" && printableText(r.Version, 32)
}

// validBuild: a build needs its resolved tool; a ready-made artifact has neither.
func (r *PluginInstallReceipt) validBuild() bool {
	if r.Build == nil || len(r.Build) > 8 {
		return false
	}
	for _, arg := range r.Build {
		if !printableText(arg, 1024) {
			return false
		}
	}
	if len(r.Build) == 0 {
		return r.Tool == "" && r.ToolSHA256 == ""
	}
	return printableText(r.Tool, 1024) && filepath.IsAbs(r.Tool) && validPluginSHA(r.ToolSHA256)
}

func (s PluginFeed) stagingRoot() string   { return filepath.Join(s.pluginDir(), ".staging") }
func (s PluginFeed) stagingPath() string   { return filepath.Join(s.stagingRoot(), s.name) }
func (s PluginFeed) pluginBinPath() string { return filepath.Join(s.pluginDir(), s.name+".bin") }

// StagingDir returns this install attempt's staging directory for source and build output.
func (i *PluginInstall) StagingDir() string { return i.feed.stagingPath() }

// KeepBuildLog keeps the staging folder after Close so the owner can read the log.
func (i *PluginInstall) KeepBuildLog() { i.keepLog = true }

// sweepPluginInstallFiles removes this name's install staging only. The
// name is validated and the parents are checked, so links are never followed.
// It runs before the copied manifest is checked, so it never touches the
// pin or the ledgers (see sweepInstallArtifacts).
func (s PluginFeed) sweepPluginInstallFiles() error {
	switch err := pluginDirectory(s.stagingRoot(), false); {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	return errors.Join(os.RemoveAll(s.stagingPath()), journal.SyncDir(s.stagingRoot()))
}

// sweepInstallArtifacts removes this name's leftover pin, reply ledger and
// start-notice ledger, only after checkInstallArtifacts proved every one of
// them is Zalki's own for current (see there); otherwise nothing is removed.
// It runs only on remove, or in Commit once the receipt and choices are
// checked and no installation of this name exists (current "").
func (s PluginFeed) sweepInstallArtifacts(current string) error {
	if err := s.checkInstallArtifacts(current); err != nil {
		return err
	}
	return errors.Join(os.RemoveAll(s.pluginBinPath()), removeIfPresent(s.replyLedgerPath()), removeIfPresent(s.startNoticePath()))
}

// BeginPluginInstall reserves a new plugin name for one install attempt.
// It refuses existing installations and abandoned data, sweeps leftovers of
// earlier failed attempts for this name, and creates an empty staging folder.
func (s PluginFeed) BeginPluginInstall() (*PluginInstall, error) {
	if err := s.checkName(); err != nil {
		return nil, err
	}
	if err := s.locked(func() error {
		if err := pluginDirectory(s.pluginDir(), true); err != nil {
			return err
		}
		return errors.Join(journal.SyncDir(filepath.Dir(s.store.Dir)), journal.SyncDir(s.store.Dir))
	}); err != nil {
		return nil, err
	}
	lease, err := s.AcquirePluginLease()
	if err != nil {
		return nil, err
	}
	install := &PluginInstall{feed: s, lease: lease}
	if err := s.prepareInstall(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	return install, nil
}

func (s PluginFeed) prepareInstall() error {
	if err := s.noInstallation(); err != nil {
		return err
	}
	if err := s.sweepPluginInstallFiles(); err != nil {
		return err
	}
	if err := pluginDirectory(s.stagingRoot(), true); err != nil {
		return err
	}
	return pluginDirectory(s.stagingPath(), true)
}

// noInstallation refuses a name that is installed or has abandoned data.
func (s PluginFeed) noInstallation() error {
	return bus.WithJournalRead(s.store, func(bus.JournalView) error { return s.noInstallationUnlocked() })
}

func (s PluginFeed) noInstallationUnlocked() error {
	p, _, err := s.loadPlugin()
	if err != nil {
		return err
	}
	if p.ID != "" {
		return model.NewError(model.CodeConflict, "plugin already installed; remove it first")
	}
	return s.checkEmptyPluginData()
}

// checkEmptyPluginData refuses to adopt data left by an earlier installation.
// An absent folder is empty; a present one must be a private directory.
func (s PluginFeed) checkEmptyPluginData() error {
	if err := pluginDirectory(s.pluginDataPath(), false); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	data, err := os.Open(s.pluginDataPath())
	if err != nil {
		return err
	}
	entries, err := data.Readdirnames(1)
	closeErr := data.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) != 0 {
		return model.NewError(model.CodeConflict, "remove abandoned plugin data before adding")
	}
	return nil
}

// Close releases the name lease. Staging is removed unless a log was kept.
func (i *PluginInstall) Close() error {
	var err error
	if !i.keepLog {
		err = os.RemoveAll(i.feed.stagingPath())
	}
	return errors.Join(err, i.lease.Close())
}

// copyPluginArtifact copies a staged regular file to <name>.bin/<sha256>.
func (s PluginFeed) copyPluginArtifact(source string) (string, error) {
	return copyPluginArtifactObserved(s, source, nil)
}

func copyPluginArtifactObserved(s PluginFeed, source string, observe func(*os.File)) (string, error) {
	fd, err := syscall.Open(source, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	input := os.NewFile(uintptr(fd), source)
	if observe != nil {
		observe(input)
	}
	defer input.Close() //nolint:errcheck // Read-only artifact copy.
	if info, err := input.Stat(); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", errors.Join(err, model.NewError(model.CodeInvalidInput, "plugin artifact must be a regular executable file, not a link"))
	}
	if err := pluginDirectory(s.pluginBinPath(), true); err != nil {
		return "", err
	}
	output, err := os.CreateTemp(s.pluginBinPath(), ".artifact-")
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, err = io.Copy(io.MultiWriter(output, hash), input)
	err = errors.Join(err, output.Chmod(0500), output.Sync(), output.Close())
	target := filepath.Join(s.pluginBinPath(), hex.EncodeToString(hash.Sum(nil)))
	if err == nil {
		err = os.Rename(output.Name(), target)
	}
	if err != nil {
		_ = os.Remove(output.Name())
		return "", err
	}
	return target, journal.SyncDir(s.pluginBinPath())
}

// savePluginInstallRecord is the commit point; tests replace it to inject a
// failure after the rename.
var savePluginInstallRecord = PluginFeed.savePlugin

// Commit copies the built artifact, pins it, takes the Store lock, verifies the
// pin again and then publishes one enabled record with scope all, the
// owner's access and the receipt in
// one atomic write. On an error with kept false nothing was published and the
// copied artifact is removed. On an error with kept true the record may be
// published (the write was not confirmed), so nothing is rolled back.
//
// receipt.Access holds the reviewed manifest's declared access. Access may be
// empty only when send or read was declared and the owner chose no session.
// Send and read need terminalReview. The installer is proven under the
// commit lock from installerActor and installerCapability (see
// installProvenance); the receipt's own InstalledBy and InstalledAt are
// overwritten.
func (i *PluginInstall) Commit(artifact string, args []string, access PluginAccess, receipt PluginInstallReceipt, installerActor, installerCapability string, terminalReview bool) (kept bool, err error) {
	if i.update != nil {
		return i.commitUpdate(artifact, args, access, receipt, installerActor, installerCapability, terminalReview)
	}
	s := i.feed
	swept := false // set only after a complete sweep: then the pin folder holds only this attempt's copy
	defer func() {
		if err != nil && !kept && swept {
			err = errors.Join(err, os.RemoveAll(s.pluginBinPath()))
		}
	}()
	access.sortLists()
	if err := commitAccessError(access, receipt.Access, terminalReview); err != nil {
		return false, err
	}
	if err := s.checkAccess(access); err != nil {
		return false, err
	}
	// Only now, with the name lease held, the choices checked and still no
	// installation of this name, may leftovers of an earlier attempt go. The
	// check reads the record without the Store lock (records are replaced
	// atomically, and the lock may be busy for long); the commit checks again
	// under the lock before it publishes anything.
	if err := s.noInstallationUnlocked(); err != nil {
		return false, err
	}
	if err := s.sweepInstallArtifacts(""); err != nil {
		return false, err // a refused or failed sweep owns nothing: the defer removes nothing
	}
	swept = true // the pin folder is now empty; what appears in it is this attempt's
	pinned, err := s.copyPluginArtifact(artifact)
	if err != nil {
		return false, err
	}
	beforePluginInstallPin()
	launch, err := NewPluginLaunch(pinned, args)
	if err != nil {
		return false, err
	}
	receipt.Build = slices.Clone(receipt.Build)
	if receipt.Build == nil {
		receipt.Build = []string{}
	}
	receipt.ArtifactSHA256 = launch.SHA256
	// Placeholders for the shape check; the commit sets the real values.
	receipt.InstalledAt, receipt.InstalledBy, receipt.InstalledByLabel = model.Timestamp(), installedByUnproven, ""
	if !receipt.valid() {
		return false, model.NewError(model.CodeInvalidInput, "plugin install receipt is invalid")
	}
	// The copy is named by the hash of the bytes copied: a change before
	// pinning shows as a different name, a change after it fails Verify.
	if filepath.Base(pinned) != launch.SHA256 {
		return false, model.NewError(model.CodeConflict, "plugin artifact changed before install")
	}
	beforePluginInstallCommit()
	err = s.write(func(view bus.JournalView) error {
		// Verify under the lock, right before the record write: no wait window.
		if err := launch.Verify(); err != nil {
			return err
		}
		receipt.InstalledAt = model.Timestamp()
		receipt.InstalledBy, receipt.InstalledByLabel = installProvenance(view, installerActor, installerCapability, terminalReview)
		kept, err = s.commitInstall(view, launch, access, receipt)
		return err
	})
	return kept, err
}

// commitAccessError checks the granted access against the declared access:
// every granted kind was declared, send and read need a reviewed terminal,
// and no access at all is allowed only when send or read was declared and
// the owner chose no session for them.
func commitAccessError(access PluginAccess, raw json.RawMessage, terminalReview bool) error {
	declared, ok := decodeDeclaredAccess(raw)
	switch {
	case !ok:
		return model.NewError(model.CodeInvalidInput, "plugin install receipt is invalid")
	case declared.extended() && !terminalReview:
		return model.NewError(model.CodeInvalidInput, "send and read need the review screen in your own terminal")
	case !declared.covers(access):
		return model.NewError(model.CodeInvalidInput, "an access choice asks for more than the plugin declared")
	case access.empty() && !declared.extended():
		return model.NewError(model.CodeInvalidInput, "select some access")
	}
	return nil
}

func (s PluginFeed) commitInstall(view bus.JournalView, launch PluginLaunch, access PluginAccess, receipt PluginInstallReceipt) (bool, error) {
	p, _, err := s.loadPlugin()
	if err != nil {
		return false, err
	}
	if p.ID != "" {
		return false, model.NewError(model.CodeConflict, "plugin already installed; remove it first")
	}
	if err := s.checkEmptyPluginData(); err != nil {
		return false, err
	}
	// The selected sessions are fixed IDs; each must be live now (U16/U17).
	for _, id := range slices.Concat(access.Send, access.Read) {
		if member, err := view.PluginMember(id, s.name); err != nil || !member {
			return false, model.NewError(model.CodeConflict, "a selected session is no longer live; nothing was installed")
		}
	}
	head, err := view.EventWindow()
	if err != nil {
		return false, err
	}
	p = pluginRecord{ID: model.NewUUID(), Name: s.name, Launch: &launch, Enabled: true, Scope: scopeAll, Access: access, Install: &receipt}
	p.setGap(head, "enabled")
	identity := filepath.Join(s.pluginDir(), "id-"+p.ID+".json")
	err = pluginDirectory(s.pluginDataPath(), true)
	if err == nil {
		err = s.savePluginIdentity(p.ID)
	}
	if err != nil {
		return false, errors.Join(err, removeIfPresent(identity), removeIfPresent(s.pluginDataPath()))
	}
	if _, err = savePluginInstallRecord(s, p); err == nil {
		return false, nil
	}
	// A save error can come after the rename (directory sync). Read the record
	// again under the same lock to learn whether it was published.
	current, _, readErr := s.loadPlugin()
	switch {
	case readErr != nil:
		return true, errors.Join(err, readErr, model.NewError(model.CodeConflict, "could not tell whether the plugin record was written"))
	case current.ID == p.ID:
		return true, errors.Join(err, model.NewError(model.CodeConflict, "the plugin record was written, but saving it was not confirmed"))
	}
	// Not published: the record write is the only commit point.
	return false, errors.Join(err, removeIfPresent(identity), removeIfPresent(s.pluginDataPath()))
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
