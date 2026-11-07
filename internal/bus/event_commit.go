package bus

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func persistSessionUpdate(s Store, next Session, previous model.SessionState) error {
	return persistSessionUpdateWith(s, next, previous, commitWriter{dir: s.Dir})
}

func persistSessionUpdateWith(s Store, next Session, previous model.SessionState, writer commitWriter) error {
	if previous != model.StateReady || next.State != model.StateBusy || next.Retired {
		return s.saveSession(next)
	}
	record, err := prepareSessionRecord(s, next)
	if err != nil {
		return err
	}
	prior, err := journal.ReadBytes(record.path, maxStateFileBytes)
	if err != nil {
		return err
	}
	event := model.Event{ID: newID(), Type: "session.state_changed",
		OccurredAt: next.UpdatedAt, TransactionID: newID(),
		Session: model.EventSession{ID: next.ID, Generation: next.Generation, Previous: previous, State: next.State}}
	batch, err := writer.prepare(prior, record.value, event, record.removed)
	if err != nil {
		return err
	}
	if err := s.ensureStateFormat("sessions", record.value.Schema); err != nil {
		return err
	}
	if err := ensureEventDirectories(s, orderEventDirectory, batch.Puts[2].PreviousSHA256 != ""); err != nil {
		return err
	}
	if err := writer.log().Commit(batch); err != nil {
		return fmt.Errorf("session commit failed or may require recovery: %w", err)
	}
	return nil
}

func orderEventDirectory(path string) error { return journal.OrderDirectory(path) }

func ensureEventDirectories(s Store, orderDir func(string) error, initialized bool) error {
	for _, relative := range []string{"commits", "events", "events/records"} {
		path := filepath.Join(s.Dir, relative)
		if initialized {
			if _, err := os.Lstat(path); err != nil {
				return err
			}
		}
		if err := privateDir(path); err != nil {
			return err
		}
	}
	if initialized {
		return nil
	}
	// Parent namespace links must precede the fully durable intent. Empty child
	// directories need no full sync; a failed attempt repeats these barriers.
	if err := orderDir(filepath.Join(s.Dir, "events")); err != nil {
		return err
	}
	return orderDir(s.Dir)
}

func eventStorePresent(s Store) bool {
	_, err := os.Lstat(filepath.Join(s.Dir, "commits"))
	return !errors.Is(err, os.ErrNotExist)
}

func recoverEventCommit(s Store) error {
	_, err := os.Lstat(filepath.Join(s.Dir, "commits", "pending.json"))
	if errors.Is(err, os.ErrNotExist) {
		if !eventStorePresent(s) {
			return nil
		}
		return (commitWriter{dir: s.Dir}).recover()
	}
	if err != nil {
		return err
	}
	return (commitWriter{dir: s.Dir}).recover()
}

func eventIntentPresent(s Store) bool {
	_, err := os.Lstat(filepath.Join(s.Dir, "commits", "pending.json"))
	return !errors.Is(err, os.ErrNotExist)
}
