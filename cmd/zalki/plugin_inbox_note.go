package main

import (
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// maxInboxIndexNotes keeps the doctor notes short when many lists are damaged.
const maxInboxIndexNotes = 8

// inboxIndexNotes names every plugin inbox list that Zalki cannot read, with
// the file and what to do. It changes nothing.
func inboxIndexNotes(store bus.Store) []string {
	entries, err := os.ReadDir(filepath.Join(store.Dir(), "plugins"))
	if err != nil {
		return nil
	}
	var notes []string
	for _, entry := range entries {
		match := pluginRecordName.FindStringSubmatch(entry.Name())
		if match == nil || !entry.Type().IsRegular() {
			continue
		}
		if problem := pluginfeed.NewPluginFeed(store, match[1]).InboxIndexProblem(); problem != "" {
			if notes = append(notes, problem); len(notes) == maxInboxIndexNotes {
				break
			}
		}
	}
	return notes
}
