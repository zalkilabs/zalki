package bus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/statefile"
)

type pluginMemberGuard struct {
	Generation string `json:"generation"`
	Audit      string `json:"audit"`
	Owner      string `json:"owner"`
}

type pluginNativeProjection struct {
	ID         string                   `json:"id"`
	Kind       string                   `json:"kind"`
	Class      string                   `json:"class"`
	Row        json.RawMessage          `json:"row,omitempty"`
	Guard      pluginMemberGuard        `json:"guard"`
	Digest     string                   `json:"digest"`
	Retirement *pluginRetirementWitness `json:"retirement,omitempty"`
}

type pluginMemberCollection struct {
	Records []pluginNativeProjection `json:"records"`
}

// PluginMembers is the trusted targeted feed projection of PluginMember.
// A nonnil target slice reads only those IDs; empty reads no native file.
// Private guards are never child wire output. charge is the pure shared budget
// for this entire borrowed write-view step.
func PluginMembers(ctx context.Context, view JournalView, pluginName, ownerScope string, ownerStatus bool, targets []string, charge func(int, int, int, int, int) error) (json.RawMessage, error) {
	if ctx == nil || charge == nil || targets == nil || !validSessionPluginName(pluginName) {
		return nil, statefile.ErrInvalidFile
	}
	store, cache, done, err := view.acquirePluginMembers()
	if err != nil {
		return nil, err
	}
	defer done()
	if !view.scope.writable {
		return nil, errJournalViewInactive
	}
	work := &pluginViewWork{ctx: ctx, charge: charge}
	if err := work.check(0, 0, 0, 0, 0); err != nil {
		return nil, err
	}
	cache.work.Store(work)
	result := pluginMemberCollection{Records: []pluginNativeProjection{}}
	if !ownerStatus || ownerScope != "all" {
		return json.Marshal(result)
	}
	ids, err := targetedPluginMemberIDs(store, targets)
	if err != nil {
		return nil, err
	}
	result, err = collectPluginMemberProjections(view, store, cache, pluginName, ids, work)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}

func collectPluginMemberProjections(view JournalView, store Store, cache *pluginMemberCache, pluginName string, ids []string, work *pluginViewWork) (pluginMemberCollection, error) {
	result := pluginMemberCollection{Records: []pluginNativeProjection{}}
	for _, id := range ids {
		if err := work.check(0, 0, 0, 0, 0); err != nil {
			return pluginMemberCollection{}, err
		}
		projection, err := projectPluginMember(view, store, cache, pluginName, id)
		if err != nil {
			return pluginMemberCollection{}, err
		}
		result.Records = append(result.Records, projection)
		if err := work.check(0, 0, 0, 0, 0); err != nil {
			return pluginMemberCollection{}, err
		}
	}
	return result, nil
}

func targetedPluginMemberIDs(store Store, targets []string) ([]string, error) {
	if len(targets) > 64 {
		return nil, statefile.ErrInvalidFile
	}
	ids := slices.Clone(targets)
	slices.Sort(ids)
	ids = slices.Compact(ids)
	for _, id := range ids {
		if _, err := store.sessionPath(id); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func projectPluginMember(view JournalView, store Store, cache *pluginMemberCache, plugin, id string) (pluginNativeProjection, error) {
	allowed, err := view.PluginMember(id, plugin)
	if err != nil {
		return pluginNativeProjection{}, err
	}
	entry := cache.native(store, id)
	kind, _, _ := strings.Cut(id, ":")
	projection := pluginNativeProjection{ID: id, Kind: kind, Class: "absent"}
	if errors.Is(entry.err, os.ErrNotExist) {
		return projection, nil
	}
	if entry.err != nil {
		return projection, entry.err
	}
	projection.Guard = guardPluginMember(entry)
	projection.Digest = hex.EncodeToString(entry.digest[:])
	if !entry.live {
		projection.Retirement = currentPluginRetirement(store.dir, id, plugin, entry)
		return projection, nil
	}
	if !entry.record.State.Valid() || entry.record.State == model.StateRetired || !model.IsCanonicalUUID(entry.record.Generation) {
		return projection, statefile.ErrInvalidFile
	}
	projection.Class = "muted"
	if allowed {
		projection.Class = "active"
	}
	projection.Row, err = encodePluginMemberRow(entry.record, !allowed)
	return projection, err
}

func guardPluginMember(entry *pluginMemberEvidence) pluginMemberGuard {
	owner, _ := json.Marshal(entry.record.Owner)
	digest := sha256.Sum256(owner)
	guard := pluginMemberGuard{Generation: entry.record.Generation, Owner: hex.EncodeToString(digest[:])}
	if entry.policyValid {
		guard.Audit = entry.policy.LastChange.ID
	}
	return guard
}

func encodePluginMemberRow(record model.Session, muted bool) (json.RawMessage, error) {
	if muted {
		return json.Marshal(struct {
			ID    string `json:"id"`
			Kind  string `json:"kind"`
			Muted bool   `json:"muted"`
		}{record.ID, record.Kind, true})
	}
	row := struct {
		ID         string             `json:"id"`
		Kind       string             `json:"kind"`
		Generation string             `json:"generation"`
		State      model.SessionState `json:"state"`
		Muted      bool               `json:"muted"`
		Name       string             `json:"name,omitempty"`
		Task       string             `json:"task,omitempty"`
	}{ID: record.ID, Kind: record.Kind, Generation: record.Generation, State: record.State, Name: pluginMemberDisplay(record.Name), Task: pluginMemberDisplay(record.Task)}
	raw, err := json.Marshal(row)
	if len(raw) > 4096 {
		return nil, statefile.ErrInvalidFile
	}
	return raw, err
}

func pluginMemberDisplay(value string) string {
	value = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(value, "")))
	if len(value) <= 256 {
		return value
	}
	end := 253
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "…"
}
