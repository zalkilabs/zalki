package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// The first-start banner (title "Zalki"). It says only that a background
// plugin is new and what the install receipt recorded about its installer.
const (
	notifyTitle       = "Zalki"
	notifyTextSession = `new background plugin "%s", installed by session "%s"` // the label quoted
	notifyTextTerm    = `new background plugin "%s", installed by terminal`
	// A receipt without proof, and a plugin with no receipt at all, both
	// get this exact sentence: nothing is inserted.
	notifyTextUnproven = `new background plugin "%s", installed by an unverified installer`
)

// Per-plugin notification state in NAME.run.json.
const (
	notifyQueued     = "queued"
	notifyDispatched = "dispatched"
	notifyFailed     = "failed"

	notifyReasonFailed   = "notification_failed"
	notifyReasonUnknown  = "notification_dispatch_unknown"
	notifyReasonBadState = "rate_state_invalid"
)

// Owner lines about notifications (list suffixes, doctor notes and the
// supervise help).
const (
	notifyOwnerCopy      = "The first time a background plugin starts, Zalki queues one best-effort macOS notification; it may not appear, and a crash may repeat it. Zalki writes its notification bookkeeping only under its private state. The title and text go to the macOS notification service through osascript. Zalki makes no network request for it."
	listNotifyFailed     = "; notification failed"
	listNotifyPaused     = "; notifications paused"
	noteNotifyUnfinished = "Zalki notifications are paused: an earlier notification did not finish. Plugins are not affected."
	noteNotifyInvalid    = "Zalki notifications are paused: the notification state file is not valid. Plugins are not affected."
	noteNotifyResume     = "To resume notifications, reset them offline, with no other zalki command running: (1) stop the background supervisor (launchctl bootout %s, or end your zalki plugins supervise); (2) make sure that every earlier banner process and its children have ended; if you are not sure, leave notifications paused; (3) delete %s; (4) start the supervisor again (log out and in, or zalki plugins supervise). This also resets the banner limits, so a past banner may repeat."
)

// The global budget: one child at a time, at least notifySpacing apart, at
// most notifyMax in any notifyWindow, and a hold of notifyWindow after a
// new controller starts. All of it uses the monotonic clock.
const (
	notifySpacing   = 10 * time.Second
	notifyWindow    = 60 * time.Second
	notifyMax       = 6
	notifyQueueSize = 31 // plus the one intent in work: at most 32
	notifyFileLimit = 4096
)

// notifyBanner is the banner text for one installation's receipt.
func notifyBanner(name string, receipt *pluginfeed.PluginInstallReceipt) string {
	switch {
	case receipt == nil || receipt.InstalledBy == "" || receipt.InstalledBy == "unproven":
		return fmt.Sprintf(notifyTextUnproven, name)
	case receipt.InstalledBy == "terminal":
		return fmt.Sprintf(notifyTextTerm, name)
	}
	label := receipt.InstalledByLabel
	if label == "" {
		label = receipt.InstalledBy
	}
	return fmt.Sprintf(notifyTextSession, name, plainText(label))
}

// notifyRecord is plugins/.supervisor-notifications.json: the attempt times
// and the one dispatch that may be in flight. It is written before exec.
type notifyRecord struct {
	Schema   int             `json:"schema"`
	Attempts []time.Time     `json:"attempts"`
	InFlight *notifyInFlight `json:"in_flight"`
}

// notifyInFlight names the dispatch in progress and the controller
// generation (its instance UUID) that owns it.
type notifyInFlight struct {
	Token      string `json:"token"`
	Controller string `json:"controller"`
	Phase      string `json:"phase"`
	PID        int    `json:"pid,omitempty"`
	Start      string `json:"start,omitempty"`
}

func notifyPath(state string) string {
	return filepath.Join(state, "plugins", ".supervisor-notifications.json")
}

func (r notifyRecord) valid() bool {
	if r.Schema != 1 || len(r.Attempts) > notifyMax {
		return false
	}
	for _, at := range r.Attempts {
		if at.IsZero() {
			return false
		}
	}
	f := r.InFlight
	return f == nil || (model.IsCanonicalUUID(f.Token) && model.IsCanonicalUUID(f.Controller) &&
		(f.Phase == "reserved" && f.PID == 0 && f.Start == "" || f.Phase == "started" && f.PID > 0 && validHostStart(f.Start, f.PID)))
}

// loadNotifyRecord reads the record strictly. A missing file is empty; a
// file that is unsafe or invalid is an error and is kept as it is.
func loadNotifyRecord(state string) (notifyRecord, error) {
	path := notifyPath(state)
	empty := notifyRecord{Schema: 1, Attempts: []time.Time{}}
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return empty, nil
	case err != nil:
		return empty, err
	case !privateFile(info):
		return empty, errors.New("the notification state file is not a private file of this user")
	}
	raw, err := journal.ReadBytes(path, notifyFileLimit)
	if err != nil {
		return empty, err
	}
	return decodeNotifyRecord(raw)
}

// decodeNotifyRecord accepts only a strict, canonical, valid record.
func decodeNotifyRecord(raw []byte) (notifyRecord, error) {
	var record notifyRecord
	if len(raw) > notifyFileLimit || bus.DecodeStrictRecordJSON(raw, &record) != nil || record.Attempts == nil || !record.valid() || !canonicalJSON(raw, record) {
		return notifyRecord{}, errors.New("the notification state file is not valid")
	}
	return record, nil
}

// load reads the record through the plugins folder handle.
func (n *notifier) load() (notifyRecord, error) {
	if n.dir == nil {
		return notifyRecord{}, errors.New("the plugins folder is not open")
	}
	raw, err := readNotifyAt(n.dir)
	switch {
	case err != nil:
		return notifyRecord{}, err
	case raw == nil:
		return notifyRecord{Schema: 1, Attempts: []time.Time{}}, nil
	}
	return decodeNotifyRecord(raw)
}

// save writes the record through the plugins folder handle.
func (n *notifier) save(record notifyRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > notifyFileLimit {
		return errors.New("the notification state exceeds its limit")
	}
	return n.write(n.dir, raw)
}

// saveOwned changes the record only while it still holds this dispatch's
// reservation (token). Anything else is kept as it is and fences.
func (n *notifier) saveOwned(token string, record notifyRecord) bool {
	if !n.writeOwned(token, record) {
		n.fenced = notifyReasonUnknown // not ours any more, or maybe on disk: unknown
		return false
	}
	return true
}

// writeOwned is saveOwned without touching the worker's state, for the
// started save, which runs beside the child runner.
func (n *notifier) writeOwned(token string, record notifyRecord) bool {
	current, err := n.load()
	if err != nil || current.InFlight == nil || current.InFlight.Token != token || current.InFlight.Controller != n.controller {
		return false
	}
	return n.save(record) == nil
}

// notifyIntent is one first start to announce.
type notifyIntent struct{ name, installation string }

// notifyResult goes back to the controller loop, which alone writes run files.
type notifyResult struct{ name, installation, state, reason string }

// notifyBudget decides when the next child may start. mine are this
// generation's attempts on the monotonic clock; stored UTC attempts count
// only while they are in the future (the clock went back), so a forward
// clock step never frees a slot.
type notifyBudget struct {
	elapsed func() time.Duration // monotonic time since this generation started
	wall    func() time.Time
	mine    []time.Duration
}

// wait is 0 when a child may start now, else how long to wait at least.
func (b *notifyBudget) wait(stored []time.Time) time.Duration {
	now := b.elapsed()
	if now < notifyWindow {
		return notifyWindow - now // the hold after a new generation
	}
	var wait time.Duration
	count := 0
	for _, at := range b.mine {
		if now-at <= notifyWindow { // an attempt exactly one window ago still counts
			count++
		}
		if now-at < notifySpacing {
			wait = max(wait, notifySpacing-(now-at))
		}
	}
	wall := b.wall()
	for _, at := range stored {
		if at.After(wall) {
			count++
		}
	}
	if count >= notifyMax {
		wait = max(wait, time.Second)
	}
	return wait
}

// notifier is the one global dispatch worker of a controller.
type notifier struct {
	state, controller string
	budget            notifyBudget
	queue             chan notifyIntent
	results           chan notifyResult
	run               func(text string, started func(pid int)) notifyOutcome
	fenced            string   // a reason: every later intent stays queued
	dir               *os.File // state/plugins, opened once (nil: paused)
	write             func(dir *os.File, raw []byte) error
	done              chan struct{}
}

// sameResolvedDir: both paths resolve (links followed) to the same folder.
// A path that cannot be resolved is not the default state.
func sameResolvedDir(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		return false
	}
	rb, err := filepath.EvalSymlinks(b)
	return err == nil && ra == rb
}

// notifierFor is the controller's notifier. Banners go out only for the
// real default installation: macOS, the OS account home (not $HOME alone)
// and a state folder that resolves to its default state, however it was
// given.
func notifierFor(state, home, controller string) *notifier {
	switch {
	case backgroundPlatform != "darwin":
		return nil
	case home == "" || home != loginItemAccountHome() || !sameResolvedDir(state, filepath.Join(home, ".local", "state", "zalki")):
		return nil
	}
	start := time.Now()
	dir, _ := openPluginsDir(state) // nil: every attempt pauses as rate_state_invalid
	return &notifier{
		state: state, controller: controller, dir: dir, write: writeNotifyAt,
		budget:  notifyBudget{elapsed: func() time.Duration { return time.Since(start) }, wall: time.Now},
		queue:   make(chan notifyIntent, notifyQueueSize),
		results: make(chan notifyResult, 2*notifyQueueSize),
		run:     runNotifyChild,
		done:    make(chan struct{}),
	}
}

// offer hands an intent to the worker without blocking.
func (n *notifier) offer(it notifyIntent) bool {
	select {
	case n.queue <- it:
		return true
	default:
		return false
	}
}

// work is the worker loop. One intent at a time; a child always finishes
// (it is bounded) before the worker looks at ctx again.
func (n *notifier) work(ctx context.Context) {
	defer close(n.done)
	defer func() {
		if n.dir != nil {
			_ = n.dir.Close()
		}
	}()
	for {
		var it notifyIntent
		select {
		case <-ctx.Done():
			return
		case it = <-n.queue:
		}
		for {
			wait, res := n.try(it)
			if wait == 0 {
				if res != nil {
					n.results <- *res
				}
				break
			}
			select {
			case <-ctx.Done():
				return // the intent stays queued in its run file
			case <-time.After(min(wait, time.Second)):
			}
		}
	}
}

// try makes one dispatch attempt for it. wait > 0: not yet (the budget).
// A nil result: nothing to record (the installation is gone or unknown).
func (n *notifier) try(it notifyIntent) (time.Duration, *notifyResult) {
	paused := func(reason string) (time.Duration, *notifyResult) {
		return 0, &notifyResult{it.name, it.installation, notifyQueued, reason}
	}
	if n.fenced != "" {
		return paused(n.fenced)
	}
	record, err := n.load()
	switch {
	case err != nil:
		return paused(notifyReasonBadState)
	case record.InFlight != nil:
		// Not cleared by an earlier generation (or by us): it may still
		// run. Nothing clears it automatically; all dispatch pauses.
		n.fenced = notifyReasonUnknown
		return paused(n.fenced)
	}
	if wait := n.budget.wait(record.Attempts); wait > 0 {
		return wait, nil
	}
	status, err := pluginfeed.NewPluginFeed(bus.StoreAt(n.state), it.name).PluginStatus()
	if err != nil || status.ID != it.installation {
		return 0, nil // reinstalled or unreadable: never announced under another UUID
	}
	return 0, n.dispatch(it, record, notifyBanner(it.name, status.Install))
}

// dispatch reserves the attempt durably, runs the bounded child and clears
// the reservation. Any reservation that is not known to be cleared fences.
func (n *notifier) dispatch(it notifyIntent, record notifyRecord, text string) *notifyResult {
	record.Attempts = append(record.Attempts, n.budget.wall().UTC())
	if len(record.Attempts) > notifyMax {
		record.Attempts = record.Attempts[len(record.Attempts)-notifyMax:]
	}
	record.InFlight = &notifyInFlight{Token: model.NewUUID(), Controller: n.controller, Phase: "reserved"}
	now := n.budget.elapsed()
	n.budget.mine = append(slices.DeleteFunc(n.budget.mine, func(at time.Duration) bool { return now-at > notifyWindow }), now)
	unknown := &notifyResult{it.name, it.installation, notifyQueued, notifyReasonUnknown}
	if n.save(record) != nil {
		n.fenced = notifyReasonUnknown // it may be on disk: unknown, not absent
		return unknown
	}
	token, reserved := record.InFlight.Token, record
	startedRec := make(chan *notifyRecord, 1) // buffered: a late save never blocks
	out := n.run(text, func(pid int) {
		started := reserved
		started.InFlight = &notifyInFlight{Token: token, Controller: n.controller, Phase: "started", PID: pid, Start: hostStart(pid)}
		if n.writeOwned(token, started) {
			startedRec <- &started
			return
		}
		startedRec <- nil
	})
	if !out.cleaned {
		// A group member may still run, or a save was not joined: the
		// reservation stays and fences.
		n.fenced = notifyReasonUnknown
		return unknown
	}
	// cleaned means the runner joined any started save, so this never waits.
	select {
	case started := <-startedRec:
		if started == nil {
			n.fenced = notifyReasonUnknown // the started save failed or the record is not ours
			return unknown
		}
		record = *started
	default: // the child never started: the reserved record is cleared as it is
	}
	cleared := record
	cleared.InFlight = nil
	if !n.saveOwned(token, cleared) {
		return unknown // the owner sees the pause even if nothing else is queued
	}
	if !out.ok {
		return &notifyResult{it.name, it.installation, notifyFailed, notifyReasonFailed}
	}
	return &notifyResult{it.name, it.installation, notifyDispatched, ""}
}

// notifySuffix is the list suffix for a run record's notification state.
func notifySuffix(run pluginRunState, ok bool) string {
	switch {
	case !ok:
		return ""
	case run.NotificationState == notifyFailed:
		return listNotifyFailed
	case run.NotificationReason == notifyReasonUnknown || run.NotificationReason == notifyReasonBadState:
		return listNotifyPaused
	}
	return ""
}

// notifyNotes are doctor's notes about paused notifications: a state file
// that is not valid, or an in-flight dispatch that no live controller owns
// or that a plugin's record names as unfinished.
func notifyNotes(store bus.Store) []string {
	record, err := loadNotifyRecord(store.Dir())
	// The way out is a manual step (nothing clears an unfinished
	// dispatch automatically).
	service := "gui/" + strconv.Itoa(os.Getuid()) + "/" + backgroundLabel
	resume := fmt.Sprintf(noteNotifyResume, service, shellQuote(filepath.Join(store.Dir(), "plugins", notifyFileName)))
	switch {
	case err != nil:
		return []string{noteNotifyInvalid, resume}
	case record.InFlight == nil:
		return nil
	case !controllerLive(store.Dir()) || anyNotifyUnknown(store):
		return []string{noteNotifyUnfinished, resume}
	}
	return nil
}

// anyNotifyUnknown: some plugin's run record says its banner is paused
// because an earlier dispatch did not finish.
func anyNotifyUnknown(store bus.Store) bool {
	entries, err := os.ReadDir(filepath.Join(store.Dir(), "plugins"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		match := pluginRecordName.FindStringSubmatch(entry.Name())
		if match == nil {
			continue
		}
		status, err := pluginStatusForDisplay(store, match[1])
		if err != nil {
			continue
		}
		if run, err := loadRunState(store.Dir(), match[1], status.ID); err == nil && run.NotificationReason == notifyReasonUnknown {
			return true
		}
	}
	return false
}
