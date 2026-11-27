package pluginfeed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/statefile"
)

// The inbox index is Zalki's own list of the messages that wait for one
// plugin. It lives in plugins/ with the other ledgers, never in the plugin's
// data folder: every file in that folder belongs to the plugin, so no file name
// there can collide with a Zalki file.

func (s pluginFiles) inboxIndexPath() string {
	return filepath.Join(s.pluginDir(), s.name+".inbox.json")
}

// earlierInboxIndexPath is where an earlier build kept the index: inside the
// data folder. It is read only while the file in plugins/ does not exist, and
// only a valid list of this installation is taken from it.
func (s pluginFiles) earlierInboxIndexPath() string {
	return filepath.Join(s.pluginDataPath(), "inbox.json")
}

// errInboxIndexUnreadable marks the index file in plugins/ when Zalki cannot
// read it. It stays an invalid-file error, as the plain error was before.
var errInboxIndexUnreadable = fmt.Errorf("inbox index: %w", statefile.ErrInvalidFile)

// inboxIndexError names the unreadable index file in plugins/ and tells what to
// do. The file is Zalki's, never the plugin's.
func inboxIndexError(name, path string) error {
	return fmt.Errorf("the inbox list of plugin %s is in %s and Zalki cannot read it. It is Zalki's file: if you remove it, "+
		"messages that are still waiting for the plugin will not be offered (%w)", name, strconv.QuoteToASCII(path), errInboxIndexUnreadable)
}

// readInboxIndexFile reads one index file and checks its shape for this
// installation. found is false when the file does not exist.
func readInboxIndexFile(path, installation string, now time.Time) (index pluginInboxIndex, found bool, err error) {
	raw, err := statefile.ReadBytes(path, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return index, false, nil
	}
	if err != nil {
		return index, true, err
	}
	if err = statefile.DecodeStrictRecordJSON(raw, &index); err != nil {
		return index, true, err
	}
	if !validPluginInboxIndex(index, installation, now) {
		return index, true, statefile.ErrInvalidFile
	}
	return index, true, nil
}

// inboxIndexSource tells which file an index was read from.
type inboxIndexSource int

const (
	inboxIndexAbsent  inboxIndexSource = iota // no list: the caller starts an empty one
	inboxIndexCurrent                         // plugins/NAME.inbox.json
	inboxIndexEarlier                         // a list an earlier build kept in the data folder
)

// readCurrentInboxIndex reads the file in plugins/. found is false when it does
// not exist; a file that cannot be read is an error that names it.
func (s pluginFiles) readCurrentInboxIndex(installation string, now time.Time) (index pluginInboxIndex, found bool, err error) {
	if err = pluginDirectory(s.pluginDir(), false); err != nil {
		return index, false, err
	}
	index, found, err = readInboxIndexFile(s.inboxIndexPath(), installation, now)
	if err != nil {
		return pluginInboxIndex{}, true, inboxIndexError(s.name, s.inboxIndexPath())
	}
	return index, found, nil
}

// readInboxIndex reads the index: the file in plugins/ first, and only while
// that file does not exist a list of this installation in the data folder that
// passes every check of the journal. Any other file named inbox.json there is
// the plugin's own data, or a damaged list, and is not read. The first write
// (settle, admit or consume) creates the file in plugins/ and removes the
// earlier list, so after that the data folder is never read again.
func (s pluginFiles) readInboxIndex(view bus.JournalView, installation string, now time.Time) (pluginInboxIndex, inboxIndexSource, error) {
	var none pluginInboxIndex
	index, found, err := s.readCurrentInboxIndex(installation, now)
	if err != nil {
		return none, inboxIndexCurrent, err
	}
	if found {
		return index, inboxIndexCurrent, nil
	}
	if err = pluginDirectory(s.pluginDataPath(), false); errors.Is(err, os.ErrNotExist) {
		return none, inboxIndexAbsent, nil
	} else if err != nil {
		return none, inboxIndexAbsent, err
	}
	if index, kind := s.classifyEarlier(view, installation, now); kind == earlierOurs {
		return index, inboxIndexEarlier, nil
	}
	return none, inboxIndexAbsent, nil
}

// inboxIndexIsReadable reports a file in plugins/ that is a valid index of this
// installation.
func (s pluginFiles) inboxIndexIsReadable(installation string, now time.Time) bool {
	_, found, err := readInboxIndexFile(s.inboxIndexPath(), installation, now)
	return err == nil && found
}

// earlierFileKind tells what the file named inbox.json in the data folder is.
type earlierFileKind int

const (
	earlierNone        earlierFileKind = iota // no such file
	earlierOurs                               // a valid list of this installation, written by Zalki
	earlierDamagedList                        // it has the top-level shape of our list but cannot be used
	earlierPluginData                         // anything else: the plugin's own data
)

// earlierListKeys are the top-level keys of a Zalki inbox list.
var earlierListKeys = []string{"version", "installation", "entries", "quota"}

// classifyEarlier classifies the file named inbox.json in the data folder. It
// is ours only if the journal accepts it as a list of this installation: file
// shape, installation id, quota header and every entry (shape, sender kind, no
// message twice, size); the list that passes is returned. A file with the keys
// of our list that fails is a damaged list; every other file is the plugin's
// data. The loader and every other reader use this one check.
func (s pluginFiles) classifyEarlier(view bus.JournalView, installation string, now time.Time) (pluginInboxIndex, earlierFileKind) {
	index, found, err := readInboxIndexFile(s.earlierInboxIndexPath(), installation, now)
	if !found {
		return pluginInboxIndex{}, earlierNone
	}
	if err == nil {
		if filtered, filterErr := filterInboxEntries(view, index, now); filterErr == nil {
			return filtered, earlierOurs
		}
	}
	if s.earlierFileHasListKeys() {
		return pluginInboxIndex{}, earlierDamagedList
	}
	return pluginInboxIndex{}, earlierPluginData
}

// earlierFileHasListKeys reports a JSON object with all the top-level keys of a
// Zalki inbox list.
func (s pluginFiles) earlierFileHasListKeys() bool {
	raw, err := statefile.ReadBytes(s.earlierInboxIndexPath(), 1<<20)
	var object map[string]json.RawMessage
	if err != nil || json.Unmarshal(raw, &object) != nil {
		return false
	}
	for _, key := range earlierListKeys {
		if _, ok := object[key]; !ok {
			return false
		}
	}
	return true
}

// removeEarlierInboxIndex removes the earlier list after it was moved. The
// caller knows the file is a list of ours that passed classifyEarlier.
func (s pluginFiles) removeEarlierInboxIndex() error {
	if err := os.Remove(s.earlierInboxIndexPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fsutil.SyncDir(s.pluginDataPath())
}

// SettleInboxIndex makes sure the index is in plugins/ before the plugin
// starts: it moves a valid list of an earlier build there, or starts an empty
// one. The caller holds the plugin lease, so no child of this name runs. A file
// named inbox.json in the data folder that is not a list of ours is the
// plugin's data: it stays untouched, the plugin starts, and one warning is
// logged; `zalki doctor` names the file.
func (s PluginFeed) SettleInboxIndex() error {
	kind := earlierNone
	err := s.write(func(view bus.JournalView) error {
		p, _, err := s.loadPlugin()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := os.Lstat(s.inboxIndexPath()); err == nil {
			// A leftover of an earlier move: it goes only when the file in plugins/
			// reads as valid and the leftover passes every check.
			if _, leftover := s.classifyEarlier(view, p.ID, now); leftover == earlierOurs && s.inboxIndexIsReadable(p.ID, now) {
				_ = s.removeEarlierInboxIndex() // Best effort: the file in plugins/ wins either way.
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		index, err := s.loadPluginInboxIndex(view, p.ID, now)
		if err != nil {
			return err
		}
		_, kind = s.classifyEarlier(view, p.ID, now)
		return s.savePluginInboxIndex(index)
	})
	// The journal callback has ended; the log carries no name and no path.
	if err != nil || s.store.Logger() == nil {
		return err
	}
	switch kind {
	case earlierNone, earlierOurs:
		// Nothing was left behind.
	case earlierDamagedList:
		s.store.Logger().LogAttrs(context.Background(), slog.LevelWarn, "plugin.inbox.file_left", slog.String("reason", "damaged_zalki_list"))
	case earlierPluginData:
		s.store.Logger().LogAttrs(context.Background(), slog.LevelInfo, "plugin.inbox.file_left", slog.String("reason", "plugin_file"))
	}
	return nil
}

// InboxIndexProblem reports, without changing anything, the sentence that
// names an unreadable index file in plugins/, or a damaged list left in the data
// folder, or "" when there is neither. A file in the data folder that is the
// plugin's own data is not a problem.
func (s PluginFeed) InboxIndexProblem() string {
	p, _, err := s.loadPlugin()
	if s.checkName() != nil || err != nil || p.ID == "" {
		return ""
	}
	now := time.Now().UTC()
	if _, _, err = s.readCurrentInboxIndex(p.ID, now); errors.Is(err, errInboxIndexUnreadable) {
		return err.Error()
	}
	kind := earlierNone
	if bus.WithJournalRead(s.store, func(view bus.JournalView) error {
		_, kind = s.classifyEarlier(view, p.ID, now)
		return nil
	}) != nil || kind != earlierDamagedList {
		return ""
	}
	return "plugin " + s.name + " has a file named inbox.json in its data folder, " + strconv.QuoteToASCII(s.earlierInboxIndexPath()) +
		". It looks like a Zalki inbox list but cannot be used, so it is left in place and the messages in it are not offered. " +
		"If it is not yours to keep, move it out of the data folder"
}
