package bus

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func saveSessionWith(s Store, next model.Session, writer commitWriter) error {
	record, err := prepareSessionRecord(s, next)
	if err != nil {
		return err
	}
	prior, err := journal.ReadBytes(record.path, maxStateFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		prior, err = nil, nil
	}
	if err != nil {
		return err
	}
	event, err := sessionEvent(prior, record.value)
	if err != nil {
		return err
	}
	if event.ID == "" {
		return savePrivateSessionRecord(s, record, prior)
	}
	batch, err := writer.prepare(prior, record.value, event)
	if err != nil {
		return err
	}
	if err := s.ensureStateFormat("sessions", record.value.Schema); err != nil {
		return err
	}
	_, metadata, err := writer.readMetadata()
	if err != nil {
		return err
	}
	if err := ensureEventDirectories(s, orderEventDirectory, metadata != nil); err != nil {
		return err
	}
	// A prior single-file rename may be live but not durable after a failed
	// directory sync. Order that preimage before the intent's FULL commit point.
	if err := writer.orderSessionDirectory(filepath.Dir(record.path)); err != nil {
		return err
	}
	if err := writer.log().Commit(batch); err != nil {
		return fmt.Errorf("session commit failed or may require recovery: %w", err)
	}
	return nil
}

// savePrivateSessionRecord is the only zero-event session write sink. Caller
// holds the Store lock and has completed pending-intent recovery and absence.
func savePrivateSessionRecord(s Store, record sessionRecord, prior []byte) error {
	kind, _, err := sessionProjection(prior, record.value)
	if err != nil {
		return err
	}
	if kind != "" {
		return journal.ErrInvalidBatch
	}
	state, err := prepareSessionBytes(prior, record.value)
	if err != nil {
		return err
	}
	if err := s.ensureStateFormat("sessions", record.value.Schema); err != nil {
		return err
	}
	current, err := journal.ReadBytes(record.path, maxStateFileBytes)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, prior) {
		return journal.ErrBatchConflict
	}
	return journal.WriteAtomic(record.path, state)
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
