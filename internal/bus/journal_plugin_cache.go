package bus

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/zalkilabs/zalki/internal/model"
)

// Only live borrowed views are registered. The existing exclusive journal lock
// separates steps; expiry removes the final reference before unlocking it.
var pluginMemberViews sync.Map

type pluginMemberEvidence struct {
	record      model.Session
	policy      sessionPluginPolicy
	policyValid bool
	live        bool
	digest      [32]byte
	err         error
}

type pluginViewWork struct {
	ctx    context.Context
	charge func(int, int, int, int, int) error
}

type pluginMemberCache struct {
	mu      sync.Mutex
	root    string
	refs    atomic.Int64
	records map[string]*pluginMemberEvidence
	work    atomic.Pointer[pluginViewWork]
}

func (v JournalView) acquirePluginMembers() (Store, *pluginMemberCache, func(), error) {
	if v.scope == nil {
		return Store{}, nil, nil, errJournalViewInactive
	}
	v.scope.mu.Lock()
	defer v.scope.mu.Unlock()
	if v.scope.store == nil {
		return Store{}, nil, nil, errJournalViewInactive
	}
	if v.scope.members == nil {
		v.scope.members = retainPluginMembers(v.scope.store.dir)
	}
	v.scope.active.Add(1)
	return *v.scope.store, v.scope.members, v.scope.active.Done, nil
}

func retainPluginMembers(root string) *pluginMemberCache {
	for {
		fresh := &pluginMemberCache{root: root, records: map[string]*pluginMemberEvidence{}}
		fresh.refs.Store(1)
		current, loaded := pluginMemberViews.LoadOrStore(root, fresh)
		cache := current.(*pluginMemberCache)
		if !loaded || cache.retain() {
			return cache
		}
		// A released entry cannot be revived between the zero transition and
		// deletion. Replacing it also keeps the later release's delete harmless.
		pluginMemberViews.CompareAndDelete(root, cache)
	}
}

func (c *pluginMemberCache) retain() bool {
	for {
		refs := c.refs.Load()
		if refs == 0 {
			return false
		}
		if c.refs.CompareAndSwap(refs, refs+1) {
			return true
		}
	}
}

func releasePluginMembers(cache *pluginMemberCache) {
	if cache != nil && cache.refs.Add(-1) == 0 {
		pluginMemberViews.CompareAndDelete(cache.root, cache)
	}
}

func invalidatePluginMember(root, id string) {
	if value, ok := pluginMemberViews.Load(root); ok {
		cache := value.(*pluginMemberCache)
		cache.mu.Lock()
		delete(cache.records, id)
		cache.mu.Unlock()
	}
}

func (c *pluginMemberCache) native(store Store, id string) *pluginMemberEvidence {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.records[id]; ok {
		return entry
	}
	entry := &pluginMemberEvidence{}
	c.records[id] = entry
	if work := c.work.Load(); work != nil {
		if entry.err = work.check(0, 0, 1, 0, 0); entry.err != nil {
			return entry
		}
	}
	var raw []byte
	entry.record, raw, entry.err = readSessionEvidence(store, id)
	if entry.err != nil {
		return entry
	}
	entry.digest = sha256.Sum256(raw)
	entry.policy, entry.policyValid = decodeSessionPluginPolicy(entry.record)
	if entry.record.Retired {
		return entry
	}
	if work := c.work.Load(); work != nil {
		if entry.err = work.check(0, 0, 0, 0, 1); entry.err != nil {
			return entry
		}
	}
	entry.live = store.sessionLiveness(entry.record) != model.LivenessExited
	return entry
}

func readSessionEvidence(store Store, id string) (model.Session, []byte, error) {
	var record model.Session
	path, err := store.sessionPath(id)
	if err != nil {
		return record, nil, err
	}
	raw, err := readScopedPluginBytes(store, path, maxStateFileBytes)
	if err != nil {
		return record, nil, err
	}
	record, err = decodeStoredSession(store, raw)
	if err == nil && (record.ID != id || !strings.HasPrefix(id, record.Kind+":")) {
		err = model.NewError(model.CodeInvalidIdentity, "session file identity mismatch")
	}
	if err == nil {
		adapter, lookupErr := store.registry.Lookup(record.Kind)
		if lookupErr != nil {
			return record, nil, lookupErr
		}
		record.Receive = adapter.ReceiveCapabilities(record)
	}
	return record, raw, err
}

func (v JournalView) pluginMember(session, plugin string) (bool, error) {
	store, cache, done, err := v.acquirePluginMembers()
	if err != nil {
		return false, err
	}
	defer done()
	if !validSessionPluginName(plugin) {
		return false, nil
	}
	entry := cache.native(store, session)
	if errors.Is(entry.err, os.ErrNotExist) {
		return false, nil
	}
	if entry.err != nil {
		return false, entry.err
	}
	return entry.live && sessionPolicyAllowsPlugin(entry.policy, entry.policyValid, plugin), nil
}
