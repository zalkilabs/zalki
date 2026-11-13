package pluginfeed

import (
	"crypto/sha256"
	"encoding/hex"
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
}

// PluginInstall holds the name lease for one install attempt and owns its
// staging folder. Close always releases the lease and removes staging unless
// a build log was kept for the owner.
type PluginInstall struct {
	feed    PluginFeed
	lease   *os.File
	keepLog bool
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
	return err == nil && printableText(r.InstalledAt, 64) && r.validPaths() && r.validBuild() && r.validClaims()
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

// sweepPluginInstallFiles removes install leftovers of this name only. The
// name is validated and the parents are checked, so links are never followed.
func (s PluginFeed) sweepPluginInstallFiles() error {
	if err := errors.Join(os.RemoveAll(s.pluginBinPath()), removeIfPresent(s.replyLedgerPath())); err != nil {
		return err
	}
	switch err := pluginDirectory(s.stagingRoot(), false); {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	return errors.Join(os.RemoveAll(s.stagingPath()), journal.SyncDir(s.stagingRoot()))
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
	if err := bus.WithJournalRead(s.store, func(bus.JournalView) error {
		p, _, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.ID != "" {
			return model.NewError(model.CodeConflict, "plugin already installed; remove it first")
		}
		return s.checkEmptyPluginData()
	}); err != nil {
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
func (i *PluginInstall) Commit(artifact string, args []string, access PluginAccess, receipt PluginInstallReceipt) (kept bool, err error) {
	s := i.feed
	defer func() {
		if err != nil && !kept {
			err = errors.Join(err, os.RemoveAll(s.pluginBinPath()))
		}
	}()
	access.sortLists()
	if access.empty() {
		return false, model.NewError(model.CodeInvalidInput, "select some access")
	}
	if err := s.checkAccess(access); err != nil {
		return false, err
	}
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
		kept, err = s.commitInstall(view, launch, access, receipt)
		return err
	})
	return kept, err
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
