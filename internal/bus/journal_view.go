package bus

import (
	"errors"
	"path/filepath"
	"sync"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

var errJournalViewInactive = errors.New("journal view is only available within its lock callback")

// JournalView reads core records under the caller's existing journal lock.
// Go does not enforce borrowed lifetimes. The view expires when the callback
// returns or panics, and the callback must not reacquire the Store lock.
type JournalView struct{ scope *journalScope }

type journalScope struct {
	mu       sync.Mutex
	store    *Store
	writable bool
	active   sync.WaitGroup
}

// WithJournalRead preserves the compatibility and recovery rules for reads.
func WithJournalRead(store Store, fn func(JournalView) error) error {
	return withJournalView(store, true, fn)
}

// WithJournalWrite checks writer compatibility before invoking fn. The view
// provides core reads. The callback may update files owned by its package.
func WithJournalWrite(store Store, fn func(JournalView) error) error {
	return withJournalView(store, false, fn)
}

func withJournalView(store Store, readOnly bool, fn func(JournalView) error) error {
	return store.lockedMode(func() error {
		scope := &journalScope{store: &store, writable: !readOnly}
		defer scope.expire()
		return fn(JournalView{scope: scope})
	}, readOnly)
}

func (s *journalScope) expire() {
	s.mu.Lock()
	s.store = nil
	s.mu.Unlock()
	s.active.Wait()
}

func (v JournalView) acquire(writeRoot *string) (Store, func(), error) {
	if v.scope == nil {
		return Store{}, nil, errJournalViewInactive
	}
	v.scope.mu.Lock()
	defer v.scope.mu.Unlock()
	if v.scope.store == nil {
		return Store{}, nil, errJournalViewInactive
	}
	if writeRoot != nil && (!v.scope.writable || v.scope.store.Dir != *writeRoot) {
		return Store{}, nil, errJournalViewInactive
	}
	v.scope.active.Add(1)
	return *v.scope.store, v.scope.active.Done, nil
}

func (v JournalView) use(fn func(Store) error) error {
	store, done, err := v.acquire(nil)
	if err != nil {
		return err
	}
	defer done()
	return fn(store)
}

// WithWriteScope runs a synchronous write to files owned by the caller's
// package, only under the originating live write scope and exact state root.
// It exposes no Store. Expiry rejects new operations and waits for active ones
// before the journal lock is released. The callback must not reacquire it.
func (v JournalView) WithWriteScope(stateDir string, fn func() error) error {
	_, done, err := v.acquire(&stateDir)
	if err != nil {
		return model.NewError(model.CodeUnauthorized, "plugin write requires its live journal scope")
	}
	defer done()
	return fn()
}

// EventWindow is the available event range. An empty stream has no epoch.
type EventWindow struct {
	Epoch              string
	First, Last, Count uint64
}

// EventWindow reads and validates the current event metadata.
func (v JournalView) EventWindow() (window EventWindow, err error) {
	err = v.use(func(store Store) error {
		head, raw, err := (commitWriter{dir: store.Dir}).readMetadata()
		if raw == nil {
			head.Epoch = ""
		}
		window = EventWindow{Epoch: head.Epoch, First: head.First, Last: head.Last, Count: head.Count}
		return err
	})
	return window, err
}

// EventAt verifies the stored event and its expected epoch and sequence.
func (v JournalView) EventAt(epoch string, sequence uint64) (event model.Event, err error) {
	err = v.use(func(store Store) error {
		data, err := journal.ReadBytes(filepath.Join(store.Dir, eventRecordPath(sequence)), maxEventBytes)
		if err != nil {
			return err
		}
		var record eventRecord
		if err := DecodeStrictRecordJSON(data, &record); err != nil {
			return err
		}
		if err := record.validate(); err != nil {
			return err
		}
		if record.Epoch != epoch || record.Sequence != sequence {
			return journal.ErrInvalidBatch
		}
		event = record.Event
		return nil
	})
	return event, err
}

// MaxMessageRecordBytes bounds the persisted message encoding.
const MaxMessageRecordBytes = maxStateFileBytes

// CurrentMessage reads the exact current-schema record and its encoded size.
func (v JournalView) CurrentMessage(id string) (message model.Message, size int, err error) {
	err = v.use(func(store Store) error {
		var readErr error
		message, size, readErr = currentMessage(store, id)
		return readErr
	})
	return message, size, err
}

func currentMessage(store Store, id string) (model.Message, int, error) {
	if !model.IsCanonicalUUID(id) {
		return model.Message{}, 0, journal.ErrInvalidFile
	}
	raw, err := journal.ReadBytes(filepath.Join(store.Dir, "messages", id+".json"), maxStateFileBytes)
	if err != nil {
		return model.Message{}, 0, err
	}
	var message model.Message
	if err := DecodeStrictRecordJSON(raw, &message); err != nil {
		return model.Message{}, 0, err
	}
	if message.Schema != recordSchema || message.ID != id || (message.Kind != "" && message.Kind != model.NoticeKind) {
		return model.Message{}, 0, journal.ErrInvalidFile
	}
	return message, len(raw), nil
}

// NativeInboxMessage projects a record against its current native recipient.
// The caller owns authorization and the audit that precedes any body release.
func (v JournalView) NativeInboxMessage(session, id string) (message model.Message, outcome string) {
	if err := v.use(func(store Store) error {
		message, outcome = nativeInboxMessage(store, session, id)
		return nil
	}); err != nil {
		return model.Message{}, "unavailable"
	}
	return message, outcome
}

func nativeInboxMessage(store Store, session, id string) (model.Message, string) {
	target, err := store.session(session)
	if err != nil || !model.IsCanonicalUUID(target.Generation) {
		return model.Message{}, "unavailable"
	}
	message, _, err := currentMessage(store, id)
	if err != nil || message.To != session {
		return model.Message{}, "denied"
	}
	if message.TargetGeneration != target.Generation {
		return model.Message{}, "stale_target"
	}
	if message.Held || !message.Status.CanAcknowledge() {
		return model.Message{}, "unavailable"
	}
	if _, err := store.sessionRecordPath(message.From); err != nil {
		return model.Message{}, "unavailable"
	}
	view := inboxView(message, target, false)
	projection := model.Message{ID: view.ID, From: view.From, To: view.To, Kind: view.Kind, BodyDeferred: view.BodyDeferred}
	if view.BodyDeferred {
		return projection, "native_deferred"
	}
	if len(view.Body) > 16<<10 {
		return model.Message{}, "too_large"
	}
	projection.Body = view.Body
	return projection, "ok"
}

// CheckSessionKind validates a canonical address and its registered adapter.
// The session file need not exist.
func (v JournalView) CheckSessionKind(id string) error {
	return v.use(func(store Store) error {
		_, err := store.sessionPath(id)
		return err
	})
}

// EncodedMessageSize measures the exact bytes used for a fresh message record.
func EncodedMessageSize(message model.Message) (int, error) {
	raw, err := journal.Merge(nil, message)
	if err != nil {
		return 0, err
	}
	encoded, err := encodeStateJSON(raw)
	return len(encoded), err
}
