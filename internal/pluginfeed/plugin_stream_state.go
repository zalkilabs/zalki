package pluginfeed

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

const streamStateLimit = 524288
const streamSeenLimit = 4096

type streamFeatures struct {
	Feed bool `json:"feed"`
}

type streamGuard struct {
	Generation string `json:"generation"`
	Audit      string `json:"audit"`
	Owner      string `json:"owner"`
}

type streamRow struct {
	ID         string            `json:"id"`
	Kind       string            `json:"kind"`
	Class      string            `json:"class"`
	Row        json.RawMessage   `json:"row,omitempty"`
	Guard      streamGuard       `json:"guard"`
	Digest     string            `json:"digest"`
	Floor      pluginPosition    `json:"floor"`
	Since      *pluginPosition   `json:"since"`
	Retirement *streamRetirement `json:"retirement,omitempty"`
}

type streamSessionSeen struct {
	Guard   streamGuard        `json:"guard"`
	State   model.SessionState `json:"state"`
	Retired bool               `json:"retired"`
}

type streamMessageSeen struct {
	ID           string                      `json:"id"`
	From         string                      `json:"from"`
	To           string                      `json:"to"`
	Guards       map[string]streamDependency `json:"guards"`
	Position     pluginPosition              `json:"position"`
	Status       model.DeliveryStatus        `json:"status"`
	Held         bool                        `json:"held"`
	Acknowledged bool                        `json:"acknowledged"`
}

type streamPending struct {
	ID           string                      `json:"id"`
	Kind         string                      `json:"kind"`
	Frames       []json.RawMessage           `json:"frames"`
	After        pluginPosition              `json:"after"`
	Guards       map[string]streamDependency `json:"guards"`
	Attempted    bool                        `json:"attempted"`
	EndAttempted bool                        `json:"end_attempted"`
	SessionID    string                      `json:"session_id,omitempty"`
	SessionSeen  *streamSessionSeen          `json:"session_seen,omitempty"`
	MessageSeen  *streamMessageSeen          `json:"message_seen,omitempty"`
	ResetAfter   string                      `json:"reset_after,omitempty"`
	Retirement   *streamRetirement           `json:"retirement,omitempty"`
}

type pluginStreamState struct {
	Installation string                       `json:"installation"`
	Enable       string                       `json:"enable"`
	Revision     string                       `json:"revision"`
	Features     streamFeatures               `json:"features"`
	Run          string                       `json:"run"`
	Cursor       pluginPosition               `json:"cursor"`
	NeedCut      bool                         `json:"need_cut"`
	ResumeCut    bool                         `json:"resume_cut"`
	Rows         map[string]streamRow         `json:"rows"`
	RowOrder     []string                     `json:"row_order"`
	States       map[string]streamSessionSeen `json:"states"`
	Seen         []streamMessageSeen          `json:"seen"`
	Pending      *streamPending               `json:"pending,omitempty"`
	LastAck      string                       `json:"last_ack,omitempty"`
	LastGap      string                       `json:"last_gap,omitempty"`
	Canceled     []string                     `json:"canceled"`
	Backoff      int                          `json:"backoff"`
}

func (s pluginFiles) streamPath() string {
	return filepath.Join(s.pluginDir(), s.name+".feed-state.json")
}

func newPluginStreamState(p pluginRecord, features streamFeatures, run string) pluginStreamState {
	return pluginStreamState{Installation: p.ID, Enable: p.Epoch, Revision: p.Revision, Features: features, Run: run, NeedCut: true, Rows: map[string]streamRow{}, States: map[string]streamSessionSeen{}, Seen: []streamMessageSeen{}, Canceled: []string{}}
}

func (s pluginFiles) loadStreamState(budget *streamBudget) (pluginStreamState, bool, error) {
	var state pluginStreamState
	info, err := os.Lstat(s.streamPath())
	if errors.Is(err, os.ErrNotExist) {
		return state, false, nil
	}
	if err != nil {
		return state, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return state, false, journal.ErrInvalidFile
	}
	raw, err := budget.read(s.streamPath(), streamStateLimit)
	if err != nil {
		return state, false, err
	}
	if err := decodeStreamJSON(raw, &state); err != nil {
		return state, false, err
	}
	state.sanitizeStreamEvidence()
	if err := state.validate(); err != nil {
		return state, false, err
	}
	return state, true, nil
}

func (s pluginFiles) saveStreamState(state *pluginStreamState) error {
	if len(state.Seen) > streamSeenLimit {
		state.Seen = state.Seen[len(state.Seen)-streamSeenLimit:]
	}
	if err := state.validate(); err != nil {
		return err
	}
	raw, err := encodeStreamState(state)
	if err != nil {
		return err
	}
	// Encoding may evict cache entries. A still-pending historical claim must
	// remain backed by its ACK proof in the exact image about to be published.
	if err := state.validate(); err != nil {
		return err
	}
	return journal.WriteAtomic(s.streamPath(), raw)
}

func encodeStreamState(state *pluginStreamState) ([]byte, error) {
	return encodeStreamStateUsing(state, json.Marshal)
}

func encodeStreamStateUsing(state *pluginStreamState, marshal func(any) ([]byte, error)) ([]byte, error) {
	base := *state
	base.Seen = []streamMessageSeen{}
	raw, err := marshal(base)
	if err != nil {
		return nil, err
	}
	remaining := streamStateLimit - len(raw) - 1 // final LF is part of the bound
	if remaining < 0 {
		return nil, journal.ErrInvalidFile
	}
	first, used := len(state.Seen), 0
	for first > 0 {
		entry, err := marshal(state.Seen[first-1])
		if err != nil {
			return nil, err
		}
		size := len(entry)
		if first < len(state.Seen) {
			size++
		}
		if size > remaining-used {
			break
		}
		used += size
		first--
	}
	state.Seen = slices.Clone(state.Seen[first:])
	if state.Seen == nil {
		state.Seen = []streamMessageSeen{}
	}
	raw, err = marshal(state)
	if err != nil {
		return nil, err
	}
	if len(raw)+1 > streamStateLimit {
		return nil, journal.ErrInvalidFile
	}
	return append(raw, '\n'), nil
}

func (s pluginStreamState) validate() error {
	if !validStreamStateIdentity(s) || !validStreamStateBounds(s) {
		return journal.ErrInvalidFile
	}
	if err := validateStreamStateHistory(s); err != nil {
		return err
	}
	if err := validateStreamRows(s.Rows); err != nil {
		return err
	}
	if err := validateStreamRowOrder(s.Rows, s.RowOrder); err != nil {
		return err
	}
	if s.Pending == nil {
		return nil
	}
	if err := s.Pending.validate(); err != nil {
		return err
	}
	return validateStreamReceiptEvidence(s, s.Pending)
}

func validStreamStateIdentity(s pluginStreamState) bool {
	return model.IsCanonicalUUID(s.Installation) && model.IsCanonicalUUID(s.Enable) && validPluginSHA(s.Revision) && model.IsCanonicalUUID(s.Run) && s.Features.Feed && validStreamPosition(s.Cursor)
}

func validStreamStateBounds(s pluginStreamState) bool {
	return len(s.Rows) <= 64 && len(s.States) <= 64 && len(s.Seen) <= streamSeenLimit && len(s.Canceled) <= 32 && s.Backoff >= 0 && s.Backoff <= 60 && len(s.RowOrder) <= 64
}

func validateStreamStateHistory(s pluginStreamState) error {
	if s.LastAck != "" && !model.IsCanonicalUUID(s.LastAck) {
		return journal.ErrInvalidFile
	}
	if s.LastGap != "" && !streamEnumContains(streamGapEnums, s.LastGap) {
		return journal.ErrInvalidFile
	}
	for _, id := range s.Canceled {
		if !model.IsCanonicalUUID(id) {
			return journal.ErrInvalidFile
		}
	}

	return nil
}

func validateStreamRowOrder(rows map[string]streamRow, order []string) error {
	known := map[string]bool{}
	for _, id := range order {
		if _, exists := rows[id]; !exists || known[id] {
			return journal.ErrInvalidFile
		}
		known[id] = true
	}
	return nil
}

func (s *pluginStreamState) sanitizeStreamEvidence() {
	if s.Rows == nil {
		s.Rows = map[string]streamRow{}
	}
	if s.States == nil {
		s.States = map[string]streamSessionSeen{}
	}
	for id, proof := range s.States {
		row, exists := s.Rows[id]
		if !exists || row.Class != "active" || proof.Guard != row.Guard || !proof.State.Valid() || proof.Retired {
			delete(s.States, id)
		}
	}
	s.Seen = slices.DeleteFunc(s.Seen, func(seen streamMessageSeen) bool { return !validStreamSeenEntry(seen) })
}

func validStreamSeenEntry(seen streamMessageSeen) bool {
	if !validStreamSeenIdentity(seen) {
		return false
	}
	expected := map[string]bool{}
	for _, id := range []string{seen.From, seen.To} {
		if !model.IsPluginInboxAddress(id) {
			expected[id] = true
		}
	}
	if len(expected) != len(seen.Guards) {
		return false
	}
	for id, dependency := range seen.Guards {
		if !validStreamSeenDependency(id, dependency, expected) {
			return false
		}
	}
	return true
}

func validStreamSeenIdentity(seen streamMessageSeen) bool {
	return model.IsCanonicalUUID(seen.ID) && model.ValidateSessionAddress(seen.From) == nil && model.ValidateSessionAddress(seen.To) == nil && seen.Status.Valid() && validStreamPosition(seen.Position) && len(seen.Guards) > 0 && len(seen.Guards) <= 2
}

func validStreamSeenDependency(id string, dependency streamDependency, expected map[string]bool) bool {
	if !expected[id] || model.ValidateSessionAddress(id) != nil || model.IsPluginInboxAddress(id) {
		return false
	}
	if dependency.Class != "active" && dependency.Class != "muted" && dependency.Class != "absent" {
		return false
	}
	if dependency.Class != "absent" && (!model.IsCanonicalUUID(dependency.Guard.Generation) || !validPluginSHA(dependency.Guard.Owner)) {
		return false
	}
	return dependency.Class != "active" || model.IsCanonicalUUID(dependency.Guard.Audit)
}

func validateStreamRows(rows map[string]streamRow) error {
	for id, row := range rows {
		if !validStreamRowIdentity(id, row) || !validStreamRowGuard(row) {
			return journal.ErrInvalidFile
		}
		if !matchingStreamRowWire(id, row) {
			return journal.ErrInvalidFile
		}
		if err := validateStreamRow(row.Row); err != nil {
			return err
		}
	}
	return nil
}

func validStreamRowIdentity(id string, row streamRow) bool {
	return row.Retirement == nil && id == row.ID && model.ValidateSessionAddress(id) == nil && !model.IsPluginInboxAddress(id) && validStreamPosition(row.Floor) && row.Since != nil && validStreamPosition(*row.Since)
}

func validStreamRowGuard(row streamRow) bool {
	if row.Class != "active" && row.Class != "muted" {
		return false
	}
	return model.IsCanonicalUUID(row.Guard.Generation) && validPluginSHA(row.Guard.Owner) && validPluginSHA(row.Digest) && (row.Guard.Audit == "" || model.IsCanonicalUUID(row.Guard.Audit))
}

func matchingStreamRowWire(id string, row streamRow) bool {
	var wire streamWireRow
	if json.Unmarshal(row.Row, &wire) != nil || wire.ID != id || wire.Kind != row.Kind || wire.Muted != (row.Class == "muted") {
		return false
	}
	return row.Class != "active" || (row.Guard.Audit != "" && wire.Generation == row.Guard.Generation)
}

func (p streamPending) validate() error {
	if !validStreamPendingBounds(p) {
		return journal.ErrInvalidFile
	}
	if p.EndAttempted && !p.Attempted {
		return journal.ErrInvalidFile
	}
	bytes := 0
	for _, frame := range p.Frames {
		if err := validateStreamFrame(frame); err != nil {
			return err
		}
		bytes += len(frame) + 1
	}
	if bytes > streamBatchLimit {
		return journal.ErrInvalidFile
	}
	return validateStreamSingleReceipt(p)
}

func validStreamPendingBounds(p streamPending) bool {
	return model.IsCanonicalUUID(p.ID) && validStreamPosition(p.After) && len(p.Frames) == 1 && len(p.Guards) <= 64
}

func validateStreamSingleReceipt(p streamPending) error {
	if len(p.Frames) != 1 || (p.Kind != "gap" && p.Kind != "event") {
		return journal.ErrInvalidFile
	}
	var frame streamFrame
	if err := json.Unmarshal(p.Frames[0], &frame); err != nil || frame.ID != p.ID {
		return journal.ErrInvalidFile
	}
	if (p.Kind == "gap") != (frame.Gap != "") || (p.Kind == "event") != (frame.Event != nil) {
		return journal.ErrInvalidFile
	}
	return nil
}

func (s *pluginStreamState) cancelPending() {
	if p := s.Pending; p != nil && p.Attempted {
		if !slices.Contains(s.Canceled, p.ID) {
			s.Canceled = append(s.Canceled, p.ID)
		}
		if len(s.Canceled) > 32 {
			s.Canceled = s.Canceled[len(s.Canceled)-32:]
		}
	}
	s.Pending = nil
}

func (s *pluginStreamState) clearEvidence() {
	s.Rows = map[string]streamRow{}
	s.RowOrder = nil
	s.States = map[string]streamSessionSeen{}
	s.Seen = []streamMessageSeen{}
}
