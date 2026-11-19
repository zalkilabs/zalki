package pluginfeed

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestStreamLedgerEvictsOldestWithBoundedEncodingWork(t *testing.T) {
	feed, _, from, _, _ := streamPendingMessageFixture(t)
	state := readStreamCheckpoint(t, feed)
	owner := feedRecordNamed(t, feed)
	position := state.Pending.After
	state.Pending = nil
	row := state.Rows[from.ID]
	for range streamSeenLimit {
		state.Seen = append(state.Seen, streamMessageSeen{ID: model.NewUUID(), From: row.ID, To: "plugin:" + owner.ID, Guards: map[string]streamDependency{row.ID: {Guard: row.Guard, Class: row.Class}}, Position: position, Status: model.StatusSubmitted})
	}
	original := state.Seen
	counter := &streamLedgerEncodingCount{}
	raw, err := encodeStreamStateUsing(&state, counter.marshal)

	if err != nil || counter.full != 2 || counter.entries > streamSeenLimit {
		t.Fatal("F_LEDGER_ENCODING_WORK_LINEAR_AND_BOUNDED", counter.full, counter.entries, err)
	}
	if len(raw) > streamStateLimit || len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Fatal("F_LEDGER_EXACT_STATE_BYTES_INCLUDE_LF")
	}
	if len(state.Seen) == 0 || len(state.Seen) >= len(original) || state.Seen[0].ID != original[len(original)-len(state.Seen)].ID || state.Seen[len(state.Seen)-1].ID != original[len(original)-1].ID {
		t.Fatal("F_LEDGER_NEWEST_SUFFIX_RETAINED")
	}
	var decoded pluginStreamState
	if err := decodeStreamJSON(raw, &decoded); err != nil || len(decoded.Seen) != len(state.Seen) {
		t.Fatal("F_LEDGER_BOUNDED_IMAGE_ROUNDTRIP", err)
	}
}

type streamLedgerEncodingCount struct{ full, entries int }

func (c *streamLedgerEncodingCount) marshal(value any) ([]byte, error) {
	switch value.(type) {
	case pluginStreamState, *pluginStreamState:
		c.full++
		if c.full > 2 {
			return nil, errors.New("counted full-state encoding exceeded two passes")
		}
	case streamMessageSeen:
		c.entries++
	}
	return json.Marshal(value)
}
