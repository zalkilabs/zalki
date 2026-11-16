package install

import (
	"context"
	"errors"
	"fmt"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

// relaunchLoginItem moves the owned item from its launcher B1 to the
// requested launcher B2 inside one recorded operation: bootout of the owned
// B1 job, the B2 plist, the B2 record, then a bootstrap. The owner's consent
// is the caller's (one question); this code only refuses or changes what it
// owns. A pending relaunch to B2 is resumed from what is observed now.
func relaunchLoginItem(ctx context.Context, p loginItemPaths, binary string) error {
	files := readLoginItemFiles(p)
	if files.pending != nil {
		if files.pending.Action != "relaunch" {
			return errLoginItemPending
		}
		return resumeRelaunch(ctx, p, files, binary)
	}
	if err := refuseFiles(files); err != nil {
		return err
	}
	record := files.record
	if files.state() != itemManaged || record == nil {
		return errInvalidLoginRecord // only an owned item is ever relaunched
	}
	if record.Binary == binary {
		// Already on this launcher: nothing is started, so the caller must
		// never report a start. The owner was shown another state.
		return errLoginItemChanged
	}
	// The B1 job must be ours or positively absent, and the OS must not have
	// disabled the label; anything else is refused before a write.
	obs := observeService(ctx, record.Binary, p.state)
	switch {
	case !obs.disabledKnown:
		return errLoginItemUnknown
	case obs.state == serviceDisabled:
		return errLoginItemDisabled
	}
	if _, err := stopPlan(obs); err != nil {
		return err
	}
	desired, err := desiredLoginItem(p, binary, record)
	if err != nil {
		return err
	}
	token, err := beginOperation(p, "relaunch", "prepared", record, desired)
	if err != nil {
		return err
	}
	return finishErr(p, token, relaunchSteps(ctx, p, record, desired, token))
}

// resumeRelaunch finishes an unfinished relaunch, but only toward its own
// recorded launcher: a request for another one (B3) changes nothing.
func resumeRelaunch(ctx context.Context, p loginItemPaths, files loginItemFiles, binary string) error {
	pending := files.pending
	if pending.Desired.Binary != binary {
		return relaunchOtherLauncher(pending.Desired.Binary, binary)
	}
	if err := refuseFiles(files); err != nil {
		return err
	}
	token, err := beginOperation(p, "relaunch", "recovering", pending.Prior, pending.Desired)
	if err != nil {
		return err
	}
	return finishErr(p, token, relaunchSteps(ctx, p, pending.Prior, pending.Desired, token))
}

// relaunchOtherLauncher names a relaunch that is recorded for another
// launcher than the one requested now.
func relaunchOtherLauncher(recorded, requested string) error {
	return fmt.Errorf("The zalki login item is being changed to %s, not %s; nothing was changed.", recorded, requested) //nolint:revive // An exact owner sentence.
}

// relaunchSteps runs (or resumes) the relaunch from fresh evidence: stop the
// owned B1 job and confirm it stopped, write the B2 plist, commit the B2
// record, then start it. Each file step is fenced by stillOurs.
func relaunchSteps(ctx context.Context, p loginItemPaths, prior, desired *loginItemRecord, token string) error {
	if err := stopForRelaunch(ctx, p, prior, token); err != nil {
		return err
	}
	if err := stillOurs(p, token); err != nil {
		return err
	}
	if files := readLoginItemFiles(p); files.plist == nil || sha256Hex(files.plist) != desired.PlistSHA {
		if err := fsutil.WriteAtomic(p.plist, renderLoginItem(desired.Binary, p.state)); err != nil {
			return err
		}
	}
	if err := commitLoginItem(p, desired, token); err != nil {
		return err
	}
	return startOwned(ctx, p, desired, "bootstrap")
}

// stopForRelaunch boots out the owned B1 job if it is loaded and confirms
// that it is gone; a failed or unconfirmed stop keeps the operation and
// writes no plist.
func stopForRelaunch(ctx context.Context, p loginItemPaths, prior *loginItemRecord, token string) error {
	obs := observeService(ctx, prior.Binary, p.state)
	if !obs.disabledKnown || obs.state == serviceDisabled {
		return errLoginItemUnknown
	}
	bootout, err := stopPlan(obs)
	if err != nil || !bootout {
		return err
	}
	if _, err := runOwnedMutation(ctx, p, token, "bootout", domain()+"/"+loginItemLabel); err != nil {
		return err
	}
	if after := observeService(ctx, prior.Binary, p.state); !after.absent {
		return errors.New("the login item stop was not confirmed; it was kept")
	}
	return nil
}
