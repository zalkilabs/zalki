package bus

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// Open-message markers let a hook read only the messages that may still need
// it, instead of every message file. The directory holds one empty file per
// message ID. Message files are never deleted or moved for this.
//
// Invariant: every open message has a marker (markers ⊇ open records).
//   - saveMessageRecord, the only writer of message files, makes the marker
//     durable before it writes an open record, and removes the marker only
//     after it writes a record that is not open. A crash can leave an extra
//     marker, never a missing one.
//   - A reader removes a marker whose record is missing or not open. It uses
//     the exact read it already did, so this adds no extra read. It first
//     syncs messages/, because the closed record it read may be visible but
//     not yet durable (its writer's directory sync failed).
//   - The manual zalki prune rebuilds the markers from a full scan, in case the
//     directory was lost or damaged. It does this first, before any of its
//     steps read the markers.
const openMessagesDir = "open-messages"

// openMessage reports whether a hook may still act on msg: it waits for hook
// context, or it is held in the approval queue.
func openMessage(msg model.Message) bool {
	return (model.HookPendingStatus(msg.Status) && msg.AcknowledgedAt == "") || msg.IsQueuedForApproval()
}

// openIndex holds the marker operations. It is a separate type so the
// markers add no methods to Store.
type openIndex struct{ store Store }

// markers returns the marker operations for s.
func markers(s Store) openIndex { return openIndex{s} }

// syncOpenMarkers syncs a directory for the markers. Tests replace it to check
// that the sync happens.
var syncOpenMarkers = journal.SyncDir

// markerName reports whether name is a canonical message ID.
func markerName(name string) bool {
	id, err := model.UUID(name)
	return err == nil && id == name
}

func (x openIndex) dir() string {
	return filepath.Join(x.store.Dir, openMessagesDir)
}

func (x openIndex) path(id string) string {
	return filepath.Join(x.dir(), id)
}

// add writes the marker for id if it is missing. With durable, it then always
// syncs the marker directory and the state directory, even when the marker or
// the directory already existed: an earlier sync may have failed, or another
// writer may have made them and stalled or crashed before its sync. So when an
// open record is written, its marker and the directory's own name were synced
// in the same call, whoever created them. id is a canonical UUID, so the path
// stays inside the directory.
func (x openIndex) add(id string, durable bool) error {
	// The journal lock creates and checks the directory; a store used
	// without it gets the same private-directory check here.
	if err := privateDir(x.dir()); err != nil {
		return err
	}
	path := x.path(id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // G304: path is the marker dir plus a canonical UUID.
	created := err == nil
	if created {
		if err := f.Close(); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	if !durable {
		return nil
	}
	if err := x.sync(); err != nil {
		if created {
			// Leave no marker that may not be durable.
			_ = os.Remove(path)
		}
		return err
	}
	return nil
}

// sync makes the markers and the marker directory's own name durable.
func (x openIndex) sync() error {
	if err := syncOpenMarkers(x.dir()); err != nil {
		return err
	}
	return syncOpenMarkers(x.store.Dir)
}

// drop removes a marker that is no longer needed. A failure is only logged:
// the extra marker costs one exact read until a later reader removes it.
func (x openIndex) drop(id string) {
	if err := os.Remove(x.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		x.store.logEvent(slog.LevelWarn, "message.open_marker_failed", eventData{MessageID: id, Err: err})
	}
}

// dropStale removes markers whose records a scan read as missing or not open.
// It first syncs messages/, once, so the closed records are durable before
// their markers can disappear. If that sync fails, the markers stay.
func (x openIndex) dropStale(ids []string) {
	if len(ids) == 0 {
		return
	}
	if err := syncOpenMarkers(filepath.Join(x.store.Dir, "messages")); err != nil {
		x.store.logEvent(slog.LevelWarn, "message.open_marker_failed", eventData{MessageID: ids[0], Err: err})
		return
	}
	for _, id := range ids {
		x.drop(id)
	}
}

// names lists the marker names that are canonical message IDs.
func (x openIndex) names() ([]string, error) {
	entries, err := os.ReadDir(x.dir())
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if markerName(entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// repair rebuilds the markers from one full scan for zalki prune: every open
// record and every unreadable file gets a marker, and a marker whose record is
// missing or not open is removed. The caller holds the journal lock.
func (x openIndex) repair() error {
	keep := map[string]bool{}
	if err := x.store.eachMessage(func(id string, msg model.Message, readErr error) error {
		if markerName(id) && (readErr != nil || openMessage(msg)) {
			keep[id] = true
			return x.add(id, false)
		}
		return nil
	}); err != nil {
		return err
	}
	names, err := x.names()
	if err != nil {
		return err
	}
	var stale []string
	for _, id := range names {
		if !keep[id] {
			stale = append(stale, id)
		}
	}
	x.dropStale(stale)
	return x.sync()
}

// eachReadable is eachReadableMessage over the marked records only: it
// visits every readable one and returns the unreadable ones it skipped.
func (x openIndex) eachReadable(visit func(model.Message) error) (skippedMessages, error) {
	var skipped skippedMessages
	err := x.each(func(id string, msg model.Message, readErr error) error {
		if readErr != nil {
			skipped.add(id)
		} else if err := visit(msg); err != nil {
			return err
		}
		return nil
	})
	return skipped, err
}

// each visits the records that have a marker, in marker-name order, with the
// same callback contract as eachMessage. A missing or closed record loses its
// marker and is not visited; an unreadable one keeps its marker and is visited
// with its read error.
func (x openIndex) each(visit func(id string, message model.Message, readErr error) error) error {
	names, err := x.names()
	if err != nil {
		return err
	}
	var stale []string
	defer func() { x.dropStale(stale) }()
	for _, id := range names {
		message, readErr := x.store.message(id)
		if errors.Is(readErr, os.ErrNotExist) || (readErr == nil && !openMessage(message)) {
			stale = append(stale, id)
			continue
		}
		if err := visit(id, message, readErr); err != nil {
			return err
		}
	}
	return nil
}
