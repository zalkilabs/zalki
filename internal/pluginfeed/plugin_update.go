package pluginfeed

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// BeginPluginUpdate holds the name lease and captures the exact installed record.
// Commit refuses a record changed during review. Close removes staging only;
// data, identity, ledgers and the current pin always belong to the installation.
func (s PluginFeed) BeginPluginUpdate() (*PluginInstall, PluginFeedStatus, error) {
	status, err := s.ReadPluginUpdateStatus()
	if err != nil {
		return nil, status, err
	}
	if status.ID == "" {
		return nil, status, model.NewError(model.CodeNotFound, "%s is not installed. Use: zalki plugins install FOLDER", s.name)
	}
	lease, err := s.AcquirePluginLease()
	if err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			err = fmt.Errorf("%w%.0w", model.NewError(model.CodeConflict, "%s has a running host or child. Run: zalki plugins disable %s; wait for its running command to exit, then run: zalki plugins update %s. Never delete its lease", s.name, s.name, s.name), err)
		}
		return nil, status, err
	}
	i := &PluginInstall{feed: s, lease: lease}
	err = bus.WithJournalRead(s.store, func(bus.JournalView) error {
		p, raw, err := s.loadPlugin()
		if err != nil {
			return err
		}
		if p.ID != status.ID {
			return model.NewError(model.CodeConflict, "plugin changed before update; try again")
		}
		if p.Install == nil {
			return model.NewError(model.CodeInvalidInput, "%s was added with --exec, not installed from a folder. Remove it and install it from its folder. Nothing was changed", s.name)
		}
		i.update = slices.Clone(raw)
		status = PluginFeedStatus{Launch: p.Launch, ID: p.ID, Name: s.name, DataDir: s.pluginDataPath(), Enabled: p.Enabled, Scope: p.Scope, Access: p.Access.clone(), Revision: p.Revision, GapPending: p.GapID != "", LastGap: p.GapReason, Install: p.Install, Epoch: p.Epoch}
		return nil
	})
	if err == nil {
		err = s.prepareUpdate(status)
	}
	if err != nil {
		return nil, status, errors.Join(err, lease.Close())
	}
	return i, status, nil
}

// ReadPluginUpdateStatus reads one atomically replaced plugin record without
// taking the journal lock, creating state or running recovery. It reports a
// snapshot for update checks, never authority to build, grant or commit.
func (s PluginFeed) ReadPluginUpdateStatus() (PluginFeedStatus, error) {
	p, _, err := s.loadPlugin()
	return PluginFeedStatus{Launch: p.Launch, ID: p.ID, Name: s.name, DataDir: s.pluginDataPath(), Enabled: p.Enabled, Scope: p.Scope, Access: p.Access.clone(), Revision: p.Revision, GapPending: p.GapID != "", LastGap: p.GapReason, Install: p.Install, Epoch: p.Epoch}, err
}

func (s PluginFeed) prepareUpdate(status PluginFeedStatus) error {
	if status.Launch.Executable != filepath.Join(s.pluginBinPath(), status.Launch.SHA256) || status.Install.ArtifactSHA256 != status.Launch.SHA256 {
		return model.NewError(model.CodeConflict, "plugin receipt does not name its installed pin; nothing was changed")
	}
	if err := status.Launch.Verify(); err != nil {
		return err
	}
	if err := s.sweepUpdatePins(status.ID, status.Launch.Executable); err != nil {
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

// Validate the whole folder and ledgers before deleting anything. Ledgers are
// retained: they belong to the same identity before and after the update.
func (s PluginFeed) sweepUpdatePins(id, current string) error {
	if err := s.checkInstallArtifacts(id); err != nil {
		return err
	}
	return s.sweepOwnedUpdatePins(current)
}

func (i *PluginInstall) commitUpdate(artifact string, args []string, access PluginAccess, receipt PluginInstallReceipt, actor, capability string, reviewed bool) (bool, error) {
	var old pluginRecord
	if err := json.Unmarshal(i.update, &old); err != nil {
		return false, err
	}
	access.sortLists()
	if err := checkUpdateReview(old, access, receipt.Access, reviewed); err != nil {
		return false, err
	}
	if err := i.feed.checkAccess(access); err != nil {
		return false, err
	}
	if receipt.ID != old.Install.ID {
		return false, model.NewError(model.CodeInvalidInput, "the folder names a different plugin; nothing was changed")
	}
	launch, err := i.updateLaunch(artifact, args, old)
	if err != nil {
		return false, err
	}
	receipt.Build = slices.Clone(receipt.Build)
	if receipt.Build == nil {
		receipt.Build = []string{}
	}
	receipt.ArtifactSHA256 = launch.SHA256
	receipt.InstalledAt, receipt.InstalledBy, receipt.InstalledByLabel = model.Timestamp(), installedByUnproven, ""
	if !receipt.valid() {
		return false, model.NewError(model.CodeInvalidInput, "plugin update receipt is invalid")
	}
	beforePluginInstallCommit()
	kept, err := i.publishUpdate(launch, access, receipt, actor, capability, reviewed)
	if err == nil {
		err = i.feed.sweepUpdatePins(old.ID, launch.Executable)
	}
	return kept, err
}

// Unreviewed updates have the same narrow contract as --yes: both the old
// and new declarations and actual grants must be identical and standard-only.
func checkUpdateReview(old pluginRecord, access PluginAccess, raw json.RawMessage, reviewed bool) error {
	before, beforeOK := decodeDeclaredAccess(old.Install.Access)
	after, afterOK := decodeDeclaredAccess(raw)
	if !reviewed && (!beforeOK || !afterOK || before != after || before.extended() || after.extended() ||
		old.Access.Status != access.Status || old.Access.Receive != access.Receive ||
		len(old.Access.Send)+len(old.Access.Read)+len(access.Send)+len(access.Read) != 0) {
		return model.NewError(model.CodeInvalidInput, "changed access needs the review screen in your own terminal; without review only unchanged standard access is allowed")
	}
	// An update may retain a disabled installation's empty grants. It must
	// never enable that record or implicitly restore a declined standard kind.
	if !old.Enabled && access.empty() && afterOK && !after.extended() {
		return nil
	}
	return commitAccessError(access, raw, reviewed)
}

func (i *PluginInstall) publishUpdate(launch PluginLaunch, access PluginAccess, receipt PluginInstallReceipt, actor, capability string, reviewed bool) (bool, error) {
	kept := false
	err := i.feed.write(func(view bus.JournalView) error {
		p, raw, err := i.feed.loadPlugin()
		if err != nil {
			return err
		}
		if !bytes.Equal(raw, i.update) {
			return model.NewError(model.CodeConflict, "%s changed while you reviewed the update. Nothing was changed; run update again", i.feed.name)
		}
		if err := launch.Verify(); err != nil {
			return err
		}
		for _, id := range slices.Concat(access.Send, access.Read) {
			if live, err := view.PluginMember(id, i.feed.name); err != nil || !live {
				return model.NewError(model.CodeConflict, "a selected session is no longer live; nothing was changed")
			}
		}
		receipt.InstalledAt = model.Timestamp()
		receipt.InstalledBy, receipt.InstalledByLabel = installProvenance(view, actor, capability, reviewed)
		priorRevision := p.Revision
		p.Launch, p.Access, p.Install = &launch, access, &receipt
		if pluginRevision(p) != priorRevision {
			if err := rebindIfEnabled(view, &p); err != nil {
				return err
			}
		}
		written, err := savePluginInstallRecord(i.feed, p)
		if err == nil {
			kept = true
			return nil
		}
		kept, err = i.feed.updatePublicationError(raw, written, err)
		return err
	})
	return kept, err
}

func (i *PluginInstall) updateLaunch(artifact string, args []string, old pluginRecord) (PluginLaunch, error) {
	sha, err := pluginExecutableSHA(artifact)
	if err != nil {
		return PluginLaunch{}, err
	}
	pinned := old.Launch.Executable
	if sha != old.Launch.SHA256 {
		pinned, err = i.feed.copyPluginArtifact(artifact)
		if err != nil {
			return PluginLaunch{}, err
		}
	}
	beforePluginInstallPin()
	launch, err := NewPluginLaunch(pinned, args)
	if err == nil && (filepath.Base(pinned) != launch.SHA256 || sha != launch.SHA256) {
		err = model.NewError(model.CodeConflict, "plugin artifact changed before update")
	}
	return launch, err
}

func (s PluginFeed) updatePublicationError(prior, written []byte, cause error) (bool, error) {
	current, err := journal.ReadBytes(s.pluginPath(), 16<<10)
	text := "the update may or may not be in effect; check it with: zalki plugins show %s"
	kept := true
	if err == nil && bytes.Equal(current, written) {
		text = "the update was written, but the disk did not confirm it; it may be in effect. Check it with: zalki plugins show %s"
	} else if err == nil && bytes.Equal(current, prior) {
		text = "the update was not written; %s keeps its previous installation"
		kept = false
	}
	return kept, errors.Join(model.NewError(model.CodeUnavailable, text, s.name), cause, err)
}
