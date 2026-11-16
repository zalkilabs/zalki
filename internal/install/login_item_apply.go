package install

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"time"

	"github.com/zalkilabs/zalki/internal/fsutil"
)

// accountHome is the OS account home, independent of HOME. Tests replace it.
var accountHome = func() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", err
	}
	return current.HomeDir, nil
}

var (
	errLoginItemForeign    = errors.New("A login item file " + loginItemLabel + ".plist exists and was not written by zalki; nothing was changed.")
	errLoginItemChanged    = errors.New("the zalki login item file was changed after zalki wrote it; it was kept and nothing was changed")
	errLoginItemUnsafe     = errors.New("the login item file is a link or not a private file of this user; nothing was changed")
	errLoginItemDisabled   = errors.New("Background plugins are disabled for " + loginItemLabel + ". Check System Settings > General > Login Items & Extensions > Allow in the Background.")
	errLoginItemUnknown    = errors.New("Background plugin status is unknown; the login item was not changed.") //nolint:revive // An exact owner sentence.
	errLoginItemBusy       = errors.New("another login-item change is in progress; try again")
	errLoginItemPending    = errors.New("an earlier login-item change did not finish; nothing was changed")
	errLoginItemLoaded     = errors.New("a different " + loginItemLabel + " job is loaded; nothing was changed")
	errLoginItemNotOwned   = errors.New("the login item is for another account home or state; nothing was changed")
	errLoginItemLockUnsafe = errors.New("the login item's lock file is not a private file of this user; nothing was changed")
	errLoginItemGeneration = errors.New("the login item's generation cannot grow any more; nothing was changed")
	errLoginItemTakenOver  = errors.New("another login-item operation took this one over; nothing more was changed")
	errLoginItemUnowned    = errors.New("a " + loginItemLabel + " job is loaded that zalki has no record of; nothing was changed")
)

// ManagedLoginItem is the one login-item operation: inspect, install,
// upgrade or uninstall the account's dev.zalki.plugins job for the default
// state. A preview (Apply false) only reads: no directory, lock, record,
// launchctl change or notification. A change holds one operation lock for
// the whole file and launchctl transaction and writes its pending record
// before touching the plist or launchd.
func ManagedLoginItem(ctx context.Context, request Request) (Report, error) {
	report := Report{Action: request.Action, DryRun: !request.Apply}
	p, err := canonicalLoginItem(request)
	if err != nil {
		return report, err
	}
	report.Paths = []string{p.plist}
	switch request.Action {
	case "inspect", "install", "upgrade", "uninstall", "relaunch":
	default:
		return report, fmt.Errorf("unknown login item action %q", request.Action)
	}
	if !request.Apply || request.Action == "inspect" {
		return inspectLoginItem(ctx, p, request.Binary, report), nil
	}
	if !loginItemsSupported {
		return inspectLoginItem(ctx, p, request.Binary, report), errors.New("login items are supported on macOS only")
	}
	unlock, err := lockLoginItem(p)
	if err != nil {
		return report, err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, loginItemOpLimit)
	defer cancel()
	if err := expectedRecord(p, request); err != nil {
		return inspectLoginItem(ctx, p, request.Binary, report), err
	}
	switch request.Action {
	case "install":
		err = installLoginItem(ctx, p, request.Binary)
	case "upgrade":
		err = wakeLoginItem(ctx, p)
	case "relaunch":
		err = relaunchLoginItem(ctx, p, request.Binary)
	default:
		err = removeLoginItem(ctx, p)
	}
	return inspectLoginItem(ctx, p, request.Binary, report), err
}

// canonicalLoginItem accepts only the OS account home and its default
// state: a scratch HOME or a custom state can never reach the real item.
func canonicalLoginItem(request Request) (loginItemPaths, error) {
	home, err := accountHome()
	if err != nil || request.HomeDir != home || !filepath.IsAbs(home) || filepath.Clean(home) != home ||
		request.StateDir != filepath.Join(home, ".local", "state", "zalki") || !filepath.IsAbs(request.Binary) {
		return loginItemPaths{}, errLoginItemNotOwned
	}
	return newLoginItemPaths(home, request.StateDir), nil
}

func inspectLoginItem(ctx context.Context, p loginItemPaths, binary string, report Report) Report {
	files := readLoginItemFiles(p)
	report.ManagedState, report.OperationPending = files.state(), files.pending != nil
	// Only a fully validated owned record with no unfinished operation names
	// its launcher; any other state leaves it empty, never a guess.
	if report.ManagedState == itemManaged && files.pending == nil && files.record != nil {
		report.LoginItemLauncher = files.record.Binary
	}
	report.LoginItemRecord = recordToken(files)
	if files.record != nil {
		binary = files.record.Binary
	}
	obs := observeService(ctx, binary, p.state)
	report.ServiceState, report.LoadedIdentity = obs.state, obs.loaded
	return report
}

// recordToken names the item the owner can be shown: the validated owned
// record with no unfinished operation, or "none" for no item at all.
func recordToken(files loginItemFiles) string {
	switch {
	case files.pending != nil:
		return ""
	case files.state() == itemManaged && files.record != nil:
		return fmt.Sprintf("%d:%s", files.record.Generation, files.record.PlistSHA)
	case files.state() == itemMissing && files.record == nil:
		return "none"
	}
	return ""
}

// expectedRecord runs under the operation lock, before any pending write or
// launchctl call: an install or relaunch acts only on the item the owner
// was shown. The token is made here, so equality also proves its format.
// Only an unfinished operation (no token) is resumed without a binding;
// a foreign or changed item (no token) gets its own 7c refusal next.
func expectedRecord(p loginItemPaths, request Request) error {
	if request.Action != "install" && request.Action != "relaunch" {
		return nil
	}
	if recordToken(readLoginItemFiles(p)) != request.ExpectRecord {
		return errLoginItemChanged
	}
	return nil
}

// lockLoginItem holds the operation lock, waiting at most two seconds. A
// stuck holder is never unlocked by time: the caller gets "busy".
func lockLoginItem(p loginItemPaths) (func(), error) {
	if err := safeChain(p.home, filepath.Dir(p.lock), true); err != nil {
		return nil, err
	}
	fd, err := openLockFile(p.lock)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		held, err := tryLock(fd)
		if held {
			return func() { closeLock(fd) }, nil
		}
		if err != nil || time.Now().After(deadline) {
			closeLock(fd)
			return nil, errLoginItemBusy
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func newOperation() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// refuseUnowned turns every state Zalki does not own into its named
// refusal; managed and missing pass.
func refuseUnowned(files loginItemFiles) error {
	if files.pending != nil {
		return errLoginItemPending
	}
	return refuseFiles(files)
}

// refuseFiles names every file state Zalki does not own.
func refuseFiles(files loginItemFiles) error {
	switch files.state() {
	case itemForeign:
		return errLoginItemForeign
	case itemChanged:
		return errLoginItemChanged
	case itemUnsafe:
		return errLoginItemUnsafe
	case itemInvalidRecord:
		return errInvalidLoginRecord
	case itemUnknown:
		return errLoginItemUnknown
	}
	return nil
}

// installLoginItem creates the item (first setup) or starts an owned one.
// It never bootstraps over a disabled, foreign, changed or unknown job, and
// a first setup needs positive evidence that no such job is loaded.
func installLoginItem(ctx context.Context, p loginItemPaths, binary string) error {
	if pending := readLoginItemFiles(p).pending; pending != nil && pending.Action == "relaunch" {
		// The owner already agreed to this relaunch: an install for its own
		// launcher finishes it with no new question; another one is refused.
		return relaunchLoginItem(ctx, p, binary)
	}
	files, err := recoverOwnOperation(ctx, p, readLoginItemFiles(p), binary)
	if err != nil {
		return err
	}
	if err := refuseUnowned(files); err != nil {
		return err
	}
	// An owned item for another launcher path is never restarted or
	// reported as foreign: it is named, and nothing is written.
	if files.record != nil && files.record.Binary != binary {
		return anotherLauncher(files.record.Binary, binary)
	}
	obs := observeService(ctx, binary, p.state)
	switch {
	case obs.state == serviceDisabled:
		return errLoginItemDisabled
	case !obs.disabledKnown:
		return errLoginItemUnknown // an unknown disablement never allows a start
	case obs.loaded == loadedMismatch:
		return errLoginItemLoaded
	case obs.loaded == loadedMatches && files.state() != itemManaged:
		// Without an ownership record a loaded job is never adopted.
		return errLoginItemUnowned
	case obs.loaded == loadedMatches:
		return startOwned(ctx, p, files.record, "kickstart") // non-killing
	case !obs.absent:
		return errLoginItemUnknown
	}
	if files.state() == itemManaged {
		return startOwned(ctx, p, files.record, "bootstrap")
	}
	return createLoginItem(ctx, p, binary, files.record)
}

// anotherLauncher names an owned item that runs another launcher path.
func anotherLauncher(owned, requested string) error {
	return fmt.Errorf("The zalki login item runs %s, not %s; nothing was changed.", owned, requested) //nolint:revive // An exact owner sentence.
}

// desiredLoginItem is the record a setup commits for binary: generation 1,
// or one above the record it replaces.
func desiredLoginItem(p loginItemPaths, binary string, prior *loginItemRecord) (*loginItemRecord, error) {
	generation := uint64(1)
	if prior != nil {
		if prior.Generation == math.MaxUint64 {
			return nil, errLoginItemGeneration
		}
		generation = prior.Generation + 1
	}
	return &loginItemRecord{Schema: 1, UID: os.Getuid(), Home: p.home, State: p.state, Label: loginItemLabel, Path: p.plist, Binary: binary, PlistSHA: sha256Hex(renderLoginItem(binary, p.state)), Generation: generation}, nil
}

// beginOperation durably records an operation with a fresh token before
// any plist or launchd change.
func beginOperation(p loginItemPaths, action, phase string, prior, desired *loginItemRecord) (string, error) {
	token := newOperation()
	return token, writePrivateJSON(p.pending, loginItemPending{Schema: 1, Operation: token, Action: action, Phase: phase, Prior: prior, Desired: desired})
}

// stillOurs re-reads the files just before a change, a commit, a clear or
// a deletion: the pending record must still carry this operation's token,
// the plist must still be owned (or missing), and a committed record must
// be one of this operation's own records.
func stillOurs(p loginItemPaths, token string) error {
	files := readLoginItemFiles(p)
	if files.pendingErr != nil || files.pending == nil || files.pending.Operation != token {
		return errLoginItemTakenOver
	}
	if refuseFiles(files) != nil {
		return errLoginItemChanged
	}
	if r := files.record; r != nil && !sameRecord(r, files.pending.Prior) && !sameRecord(r, files.pending.Desired) {
		return errLoginItemChanged
	}
	return nil
}

func sameRecord(a, b *loginItemRecord) bool { return b != nil && *a == *b }

// clearOperation removes this finished operation's pending record, only
// while stillOurs holds (token, owned plist, own records).
func clearOperation(p loginItemPaths, token string) error {
	if err := stillOurs(p, token); err != nil {
		return err
	}
	if err := os.Remove(p.pending); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsutil.SyncDir(filepath.Dir(p.pending))
}

// finalLook is the loaded-identity fence after an OS call: our job loaded,
// or positively absent, passes; a different or unknown job does not.
func finalLook(ctx context.Context, p loginItemPaths, binary string) error {
	_, err := stopPlan(observeService(ctx, binary, p.state))
	return err
}

// The "unfinished" errors report a change that stopped while its pending
// record still carries this operation's token: something WAS written, so
// none of them says that nothing changed.
var (
	errUnfinishedLoaded  = errors.New("The background setup is unfinished (a different job is loaded); it is checked again the next time it runs.")       //nolint:revive // An exact owner sentence.
	errUnfinishedUnknown = errors.New("The background setup is unfinished (the job could not be identified); it is checked again the next time it runs.") //nolint:revive // An exact owner sentence.
	errUnfinishedChanged = errors.New("The background setup is unfinished (the login item files changed); it is checked again the next time it runs.")    //nolint:revive // An exact owner sentence.
	errUnfinished        = errors.New("The background setup is unfinished (launchd did not confirm it); it is checked again the next time it runs.")      //nolint:revive // An exact owner sentence.
)

// finishErr turns err into its "unfinished" error while this operation's
// pending record is still there; a takeover or a finished operation keeps
// its own error.
func finishErr(p loginItemPaths, token string, err error) error {
	files := readLoginItemFiles(p)
	if err == nil || errors.Is(err, errLoginItemTakenOver) || files.pending == nil || files.pending.Operation != token {
		return err
	}
	switch {
	case errors.Is(err, errUnfinishedLoaded), errors.Is(err, errUnfinishedUnknown), errors.Is(err, errUnfinishedChanged), errors.Is(err, errUnfinished):
		return err
	case errors.Is(err, errLoginItemLoaded):
		return errUnfinishedLoaded
	case errors.Is(err, errLoginItemUnknown):
		return errUnfinishedUnknown
	case errors.Is(err, errLoginItemChanged):
		return errUnfinishedChanged
	}
	return errUnfinished
}

// runOwnedMutation runs one launchctl change of this operation. A known
// exit (success or failure) ends the operation; an unknown result (no exit
// status, a timeout) keeps the pending record, so nothing new is started
// until a later run has reconciled it. A lost token is not a known result:
// the caller then neither clears nor commits.
func runOwnedMutation(ctx context.Context, p loginItemPaths, token string, args ...string) (known bool, err error) {
	if err := stillOurs(p, token); err != nil {
		return false, err // not ours any more: the caller touches nothing
	}
	known, err = runMutationKnown(ctx, args...)
	if !known {
		return false, fmt.Errorf("launchd did not answer %s; the change is kept as unfinished: %w", args[0], err)
	}
	return true, err
}

// startOwned bootstraps or kickstarts the owned job inside its own
// recorded operation, after a fresh look at the job.
func startOwned(ctx context.Context, p loginItemPaths, record *loginItemRecord, verb string) error {
	if record == nil {
		return errInvalidLoginRecord // only an owned record is ever started
	}
	token, err := beginOperation(p, "start", "prepared", record, record)
	if err != nil {
		return err
	}
	obs := observeService(ctx, record.Binary, p.state)
	args := []string{"kickstart", domain() + "/" + loginItemLabel}
	switch {
	case !obs.disabledKnown || obs.state == serviceDisabled:
		// An unknown or positive disablement never allows a start.
		if err := clearOperation(p, token); err != nil {
			return err
		}
		return errLoginItemUnknown
	case verb == "bootstrap" && obs.absent:
		args = []string{"bootstrap", domain(), p.plist}
	case verb == "kickstart" && obs.state == serviceLoaded && obs.loaded == loadedMatches:
	default:
		// The job changed since the plan: nothing is started.
		if err := clearOperation(p, token); err != nil {
			return err
		}
		return errLoginItemUnknown
	}
	known, err := runOwnedMutation(ctx, p, token, args...)
	if !known {
		return finishErr(p, token, err)
	}
	// The final fence: a fresh look after the call; only our job (or its
	// proven absence) ends the operation.
	if lookErr := finalLook(ctx, p, record.Binary); lookErr != nil {
		return finishErr(p, token, lookErr)
	}
	if clearErr := clearOperation(p, token); clearErr != nil {
		return finishErr(p, token, clearErr)
	}
	return err
}

// recoverOwnOperation is the only path that may act on an unfinished
// operation, and only under the operation lease: it takes the operation
// over with its own token, then reconciles from fresh evidence. Files that
// are not ours, or a job it cannot identify, stop it with the pending
// record kept. An uninstall is left to removeLoginItem.
func recoverOwnOperation(ctx context.Context, p loginItemPaths, files loginItemFiles, binary string) (loginItemFiles, error) {
	pending := files.pending
	if pending == nil || pending.Action == "uninstall" || pending.Action == "relaunch" {
		return files, nil // removeLoginItem and relaunchLoginItem resume their own
	}
	if err := refuseFiles(files); err != nil {
		return files, err
	}
	// An install request for another launcher never finishes this one's
	// setup: it is named, and nothing is written.
	if binary != "" && pending.Desired.Binary != binary {
		return files, anotherLauncher(pending.Desired.Binary, binary)
	}
	token, err := beginOperation(p, pending.Action, "recovering", pending.Prior, pending.Desired)
	if err != nil {
		return files, err
	}
	// The OS result of the earlier change is unknown: only a fresh look at
	// the job decides, BEFORE any file is repaired; nothing is replayed blind.
	if err := reconcile(ctx, p, files, token); err != nil {
		return files, finishErr(p, token, err)
	}
	return readLoginItemFiles(p), nil
}

// reconcile finishes a taken-over operation from fresh evidence: look at
// the job, recheck the token and files, repair, then commit or clear.
func reconcile(ctx context.Context, p loginItemPaths, files loginItemFiles, token string) error {
	pending := files.pending
	if _, err := stopPlan(observeService(ctx, pending.Desired.Binary, p.state)); err != nil {
		return err
	}
	if err := stillOurs(p, token); err != nil {
		return err
	}
	if err := recoverFiles(p, files); err != nil {
		return err
	}
	if pending.Action == "install" {
		return commitLoginItem(p, pending.Desired, token)
	}
	return clearOperation(p, token)
}

// recoverFiles brings the files to the operation's desired state, or
// refuses: an install writes a missing plist with the recorded bytes only;
// a start needs the committed record to be the one it started.
func recoverFiles(p loginItemPaths, files loginItemFiles) error {
	pending, desired := files.pending, files.pending.Desired
	if pending.Action != "install" {
		if files.record == nil || *files.record != *desired {
			return errInvalidLoginRecord
		}
		return nil
	}
	switch {
	case files.record != nil && *files.record != *desired && (pending.Prior == nil || *files.record != *pending.Prior):
		return errInvalidLoginRecord
	case files.plist == nil:
		if err := safeChain(p.home, filepath.Dir(p.plist), true); err != nil {
			return err
		}
		return fsutil.WriteAtomic(p.plist, renderLoginItem(desired.Binary, p.state))
	case sha256Hex(files.plist) != desired.PlistSHA:
		return errLoginItemChanged
	}
	return nil
}

func createLoginItem(ctx context.Context, p loginItemPaths, binary string, prior *loginItemRecord) error {
	plist := renderLoginItem(binary, p.state)
	desired, err := desiredLoginItem(p, binary, prior)
	if err != nil {
		return err // refused before any write; the prior state is kept
	}
	// The plist's folder is checked before anything is recorded or written.
	if err := safeChain(p.home, filepath.Dir(p.plist), true); err != nil {
		return err
	}
	token, err := beginOperation(p, "install", "prepared", prior, desired)
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(p.plist, plist); err != nil {
		return finishErr(p, token, err) // pending stays: the file may or may not be there
	}
	if obs := observeService(ctx, binary, p.state); !obs.absent || !obs.disabledKnown || obs.state == serviceDisabled {
		return finishErr(p, token, errLoginItemUnknown) // pending stays: a later run reconciles
	}
	known, err := runOwnedMutation(ctx, p, token, "bootstrap", domain(), p.plist)
	if !known {
		return finishErr(p, token, err)
	}
	// The final fence: a fresh look after the call; only our job (or its
	// proven absence, after a known failure) lets the file be committed.
	if lookErr := finalLook(ctx, p, binary); lookErr != nil {
		return finishErr(p, token, lookErr)
	}
	// A known answer: the file is Zalki's whatever launchd said.
	if commitErr := commitLoginItem(p, desired, token); commitErr != nil {
		return finishErr(p, token, commitErr)
	}
	if err != nil {
		return fmt.Errorf("the login item was written, but launchd did not start it: %w", err)
	}
	return nil
}

// commitLoginItem publishes ownership, then removes the pending record,
// only while stillOurs holds (token, owned plist, own records).
func commitLoginItem(p loginItemPaths, record *loginItemRecord, token string) error {
	if err := stillOurs(p, token); err != nil {
		return err
	}
	if err := writePrivateJSON(p.record, record); err != nil {
		return err
	}
	afterRecordPublished()
	// The pending record is removed through the same fence as any clear.
	return clearOperation(p, token)
}

// afterRecordPublished is a test seam between publishing the record and
// clearing the operation; it does nothing in production.
var afterRecordPublished = func() {}

// wakeLoginItem gives a loaded, matching, enabled item a non-killing
// kickstart (after an install, enable or upgrade). It never bootstraps,
// enables or repairs.
func wakeLoginItem(ctx context.Context, p loginItemPaths) error {
	files, err := recoverOwnOperation(ctx, p, readLoginItemFiles(p), "")
	if err != nil {
		return err
	}
	if files.state() != itemManaged || files.pending != nil {
		return nil
	}
	if obs := observeService(ctx, files.record.Binary, p.state); obs.state == serviceLoaded && obs.loaded == loadedMatches {
		return startOwned(ctx, p, files.record, "kickstart") // rechecks the disablement
	}
	return nil
}

// removeLoginItem stops the exact owned job, confirms it stopped, then
// deletes the owned plist and its records. Anything unconfirmed keeps the
// pending ownership; foreign or changed files are never touched.
func removeLoginItem(ctx context.Context, p loginItemPaths) error {
	files, err := recoverOwnOperation(ctx, p, readLoginItemFiles(p), "")
	if err != nil {
		return err
	}
	if files.state() == itemMissing && files.record == nil && files.pending == nil {
		return nil
	}
	record, err := uninstallTarget(files)
	if err != nil {
		return err
	}
	token, err := beginUninstall(ctx, p, record, files.pending != nil)
	if err != nil {
		if token != "" {
			return finishErr(p, token, err) // a resumed uninstall stays unfinished
		}
		return err
	}
	if err := stopLoginItem(ctx, p, token, record.Binary); err != nil {
		return finishErr(p, token, err)
	}
	if err := stillOurs(p, token); err != nil {
		return finishErr(p, token, err)
	}
	return finishErr(p, token, deleteOwnedLoginItem(p, record))
}

// uninstallTarget is the owned record an uninstall removes. A resumed
// uninstall still needs the files to be ours: the plist owned or gone,
// and a committed record that is the prior one.
func uninstallTarget(files loginItemFiles) (*loginItemRecord, error) {
	if err := refuseUnowned(files); err != nil {
		if !errors.Is(err, errLoginItemPending) || files.pending.Action != "uninstall" {
			return nil, err
		}
		if err := refuseFiles(files); err != nil {
			return nil, err
		}
		if files.record != nil && *files.record != *files.pending.Prior {
			return nil, errInvalidLoginRecord
		}
	}
	record := files.record
	if record == nil && files.pending != nil {
		record = files.pending.Prior
	}
	if record == nil {
		return nil, errInvalidLoginRecord
	}
	return record, nil
}

// beginUninstall records the uninstall. A fresh one refuses a job that is
// not ours, or not known, before anything is written; a resumed one first
// takes the operation over with its own token, then looks.
func beginUninstall(ctx context.Context, p loginItemPaths, record *loginItemRecord, resumed bool) (string, error) {
	if resumed {
		token, err := beginOperation(p, "uninstall", "recovering", record, nil)
		if err != nil {
			return "", err
		}
		_, err = stopPlan(observeService(ctx, record.Binary, p.state))
		return token, err
	}
	if _, err := stopPlan(observeService(ctx, record.Binary, p.state)); err != nil {
		return "", err
	}
	return beginOperation(p, "uninstall", "prepared", record, nil)
}

// stopPlan says whether the owned job must be booted out. A different
// loaded job is refused whether or not the OS disabled the label, and
// anything that is neither our loaded job nor positively absent is unknown.
func stopPlan(obs serviceObservation) (bool, error) {
	switch {
	case obs.loaded == loadedMismatch:
		return false, errLoginItemLoaded
	case obs.loaded == loadedMatches:
		return true, nil
	case obs.absent:
		return false, nil
	}
	return false, errLoginItemUnknown
}

func stopLoginItem(ctx context.Context, p loginItemPaths, token, binary string) error {
	bootout, err := stopPlan(observeService(ctx, binary, p.state))
	if err != nil || !bootout {
		return err
	}
	if _, err := runOwnedMutation(ctx, p, token, "bootout", domain()+"/"+loginItemLabel); err != nil {
		return fmt.Errorf("the login item was not stopped; it was kept: %w", err)
	}
	if after := observeService(ctx, binary, p.state); !after.absent {
		return errors.New("the login item stop was not confirmed; it was kept")
	}
	return nil
}

// deleteOwnedLoginItem removes the plist only when its bytes are still the
// owned ones, then the records, syncing each directory.
func deleteOwnedLoginItem(p loginItemPaths, record *loginItemRecord) error {
	if err := safeChain(p.home, filepath.Dir(p.plist), false); err != nil {
		return err
	}
	raw, err := readPrivateFile(p.plist, loginItemLimit)
	switch {
	case err == nil && sha256Hex(raw) != record.PlistSHA:
		return errLoginItemChanged
	case err == nil:
		if err := os.Remove(p.plist); err != nil {
			return err
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := fsutil.SyncDir(filepath.Dir(p.plist)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, path := range []string{p.record, p.pending} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return fsutil.SyncDir(filepath.Dir(p.record))
}
