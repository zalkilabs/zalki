package bus

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zalkilabs/zalki/internal/model"
)

// floodRecord owns the persistence header around the pure quota values.
type floodRecord struct {
	Schema  int                          `json:"schema"`
	Target  string                       `json:"target"`
	Inbound model.FloodBucket            `json:"inbound"`
	Senders map[string]model.FloodBucket `json:"senders"`
	Recent  []model.FloodArrival         `json:"recent"`
}

// floodQuota maps the persisted record to the model's quota state.
func floodQuota(record floodRecord) model.FloodState {
	return model.FloodState{Inbound: record.Inbound, Senders: record.Senders, Recent: record.Recent}
}

func withFloodQuota(record floodRecord, quota model.FloodState) floodRecord {
	record.Inbound, record.Senders, record.Recent = quota.Inbound, quota.Senders, quota.Recent
	return record
}

func (s Store) floodPath(target string) (string, error) {
	if _, err := s.sessionPath(target); err != nil {
		return "", err
	}
	return filepath.Join(s.Dir, "flood", strings.ReplaceAll(target, ":", "_")+".json"), nil
}

// consumeFloodQuota is called under the journal lock, after the exact-ID retry
// check and validation. One atomic target file contains both buckets, so a
// rejected inbound check never spends a sender token (or vice versa).
func (s Store) consumeFloodQuota(msg model.Message, now time.Time) error {
	if msg.Kind == model.NoticeKind {
		return nil
	}
	path, err := s.floodPath(msg.To)
	if err != nil {
		return err
	}
	state, err := loadFloodState(path, msg.To, now)
	if err != nil {
		return err
	}
	decision := model.DecideFloodAdmission(floodQuota(state), derivedID("zalki/flood/sender/v1", msg.From), msg.ID, now)
	switch {
	case decision.Code == model.CodeRateLimited:
		return model.FloodRetryError(decision.Reason, state.Target, now, decision.RetryAt)
	case decision.Code != "":
		return model.NewError(decision.Code, "%s", decision.Reason)
	case !decision.Changed:
		return nil
	}
	return s.saveFloodState(path, withFloodQuota(state, decision.Next))
}

// loadFloodState validates the persisted header before admission replays arrivals.
func loadFloodState(path, target string, now time.Time) (floodRecord, error) {
	var state floodRecord
	if err := readJSON(path, &state); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return state, err
		}
		state = withFloodQuota(floodRecord{Schema: 1, Target: target}, model.NewFloodState(now))
	}
	if state.Schema != 1 {
		return state, model.NewError(model.CodeUnsupportedSchema, "unsupported flood quota schema %d", state.Schema)
	}
	if state.Target != target || !floodQuota(state).ValidHeader() {
		return state, model.NewError(model.CodeInvalidInput, "invalid flood quota state")
	}
	return state, nil
}

func (s Store) saveFloodState(path string, state floodRecord) error {
	if err := privateDir(filepath.Dir(path)); err != nil {
		return err
	}
	// Reserve quota first: an IO failure or crash may consume capacity without
	// sending, but must not permit uncounted delivery after restarting.
	return s.writeRecord(path, state)
}
