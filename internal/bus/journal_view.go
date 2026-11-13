package bus

import (
	"errors"
	"path/filepath"
	"sync"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

var errJournalViewInactive = errors.New("journal view is only available within its lock callback")

// JournalView reads core records under the caller's existing exclusive journal
// lock. Go does not enforce borrowed lifetimes: the view expires when its
// originating callback returns or panics. Callbacks must not re-enter the Store
// lock, including through another WithJournalRead or WithJournalWrite call.
// Core reads through a zero or expired view return errJournalViewInactive.
type JournalView struct{ scope *journalScope }

type journalScope struct {
	mu       sync.Mutex
	store    *Store
	writable bool
	active   sync.WaitGroup
	members  *pluginMemberCache
}

// WithJournalRead runs fn under the exclusive journal lock, not a shared read
// lock. Pending event and reply recovery always runs before fn and may write to
// the journal. The view is read-only even when recovery writes; fn must not
// re-enter the Store lock.
func WithJournalRead(store Store, fn func(JournalView) error) error {
	return withJournalView(store, true, fn)
}

// WithJournalWrite takes the exclusive journal lock and recovers pending events
// and replies before invoking fn. The view provides core reads; fn may update
// files owned by its package without re-entering the Store lock. A write
// reached through an injected callback started by bus must validate its
// borrowed view with WithWriteScope.
func WithJournalWrite(store Store, fn func(JournalView) error) error {
	return withJournalView(store, false, fn)
}

func withJournalView(store Store, readOnly bool, fn func(JournalView) error) error {
	return store.locked(func() error {
		scope := &journalScope{store: &store, writable: !readOnly}
		defer scope.expire()
		return fn(JournalView{scope: scope})
	})
}

func (s *journalScope) expire() {
	s.mu.Lock()
	cache := s.members
	s.members = nil
	s.store = nil
	s.mu.Unlock()
	s.active.Wait()
	releasePluginMembers(cache)
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

// WithWriteScope runs fn synchronously to write files owned by the caller's
// package. Use it for writes reached through an injected callback started by
// bus, including each later reservation callback. It validates the existing
// scope without taking the journal lock again or exposing a Store.
//
// A zero or expired view, a read-only scope and a different state root all return
// the same CodeUnauthorized error, "plugin write requires its live journal
// scope", without invoking fn. stateDir must equal Store.Dir by exact string
// comparison; paths are not cleaned or resolved. Errors from fn pass through.
// Expiry rejects new operations and waits for active ones before releasing the
// journal lock. fn must not re-enter the Store lock.
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
	if err := decodeStoredMessage(raw, &message); err != nil {
		return model.Message{}, 0, err
	}
	if message.Schema != recordSchema || message.ID != id || (message.Kind != "" && message.Kind != model.NoticeKind) {
		return model.Message{}, 0, journal.ErrInvalidFile
	}
	return message, len(raw), nil
}

// SessionGeneration returns the current registration generation of a session.
// A session that was never registered or was removed gives os.ErrNotExist.
func (v JournalView) SessionGeneration(id string) (generation string, err error) {
	err = v.use(func(store Store) error {
		session, readErr := store.session(id)
		generation = session.Generation
		return readErr
	})
	return generation, err
}

// PluginMember reports whether a plugin may use session now: the record
// exists, is not retired, and its liveness is not exited (unknown is live).
// Liveness is the same check zalki list uses, so a plugin's members are the
// sessions the owner sees. A session that was never registered gives false
// with no error; any read failure gives false and the error, so callers fail
// closed. ZAL-6 adds the session's own plugin policy for plugin here.
func (v JournalView) PluginMember(session, plugin string) (bool, error) {
	return v.pluginMember(session, plugin)
}

// AuthenticatedSessionLabel proves actor with its capability inside this
// view, as every native command does, and returns only its display label:
// its native session name, or "" when it has none. It never returns the
// session record, its key hash or policy. A plugin address, a retired or
// exited session and a wrong capability are refused.
func (v JournalView) AuthenticatedSessionLabel(actor, capability string) (label string, err error) {
	err = v.use(func(store Store) error {
		if model.IsPluginInboxAddress(actor) || model.ValidateSessionAddress(actor) != nil {
			return model.NewError(model.CodeInvalidIdentity, "the installer must be a native session")
		}
		session, err := store.authenticate(actor, capability)
		if err != nil {
			return err
		}
		named := []model.Session{session}
		store.nativeNames(named)
		label = named[0].Name
		return nil
	})
	return label, err
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
	// A plugin reads only mail from a native session. A plugin-sent message
	// stays with its recipient: plugins never read each other's mail, and the
	// refusal looks like any other unreadable message.
	if model.IsPluginInboxAddress(message.From) {
		return model.Message{}, "unavailable"
	}
	if _, err := store.sessionPath(message.From); err != nil {
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
	encoded, err := encodeStateJSON(message)
	return len(encoded), err
}
