package pluginfeed

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
)

func checkpointImage(t *testing.T, feed PluginFeed) []byte {
	t.Helper()
	raw, err := os.ReadFile(feed.streamPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func initializeUpdateCheckpoint(t *testing.T, feed PluginFeed) {
	t.Helper()
	streamReady(t, feed, streamTestLaunch(t, feed))
}

func TestStreamPrivateCheckpointRejectsInvalidPositionsAndGap(t *testing.T) {
	for name, change := range map[string]func(*pluginStreamState){
		"bad_epoch":              func(s *pluginStreamState) { s.Cursor.Epoch = "invalid" },
		"sequence_without_epoch": func(s *pluginStreamState) { s.Cursor = pluginPosition{Sequence: 1} },
		"bad_gap_id":             func(s *pluginStreamState) { s.Pending.ID = "invalid" },
	} {
		t.Run(name, func(t *testing.T) {
			feed, launch, _, _, _ := streamPendingMessageFixture(t)
			state := readStreamCheckpoint(t, feed)
			change(&state)
			raw, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(feed.streamPath(), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, _, err := feed.loadStreamState(feed.stream.budget(t.Context())); err == nil {
				t.Fatal("F_CHECKPOINT_INVALID_POSITION_OR_GAP_REFUSED")
			}
			if _, err := feed.NextPluginFeed(t.Context(), launch, "", 0); err == nil {
				t.Fatal("F_CHECKPOINT_INVALID_POSITION_OR_GAP_REFUSED")
			}
			if !bytes.Equal(raw, checkpointImage(t, feed)) {
				t.Fatal("F_CHECKPOINT_INVALID_RECORD_NOT_REPAIRED")
			}
		})
	}
}

func TestStreamOwnerStatusReadsCheckpointWithoutMutation(t *testing.T) {
	feed, launch, _, _, offer := streamPendingMessageFixture(t)
	before := checkpointImage(t, feed)
	if checkpoint := readStreamCheckpoint(t, feed); checkpoint.LastGap != "launch" {
		t.Fatal("B_STATUS_FIXTURE_IS_EXACT_PLAIN_LAUNCH")
	}
	owner := readCommitFile(t, feed.pluginPath())
	for _, read := range []func() (PluginFeedStatus, error){feed.PluginStatus, feed.ReadPluginUpdateStatus} {
		status, err := read()
		if err != nil || status.GapPending || status.LastGap != "launch" || status.ID != launch.ID {
			t.Fatal("F_STATUS_FROM_CURRENT_CHECKPOINT", status, err)
		}
	}
	if !bytes.Equal(before, checkpointImage(t, feed)) || !bytes.Equal(owner, readCommitFile(t, feed.pluginPath())) {
		t.Fatal("F_STATUS_DOES_NOT_MUTATE_OWNER_OR_CHECKPOINT")
	}
	if _, err := feed.NextPluginFeed(t.Context(), launch, offer.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := feed.RemovePlugin(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(feed.streamPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("F_REMOVE_CLEARS_PRIVATE_CHECKPOINT", err)
	}
}

func TestStreamOwnerCursorFieldsAreLegacyAndNeverMigrated(t *testing.T) {
	for _, field := range []string{"position", "gap_id", "gap_reason"} {
		t.Run(field, func(t *testing.T) {
			feed, _, _, _, _ := streamPendingMessageFixture(t)
			before := checkpointImage(t, feed)
			var owner map[string]json.RawMessage
			if err := json.Unmarshal(readCommitFile(t, feed.pluginPath()), &owner); err != nil {
				t.Fatal(err)
			}
			owner[field] = json.RawMessage(`null`)
			raw, err := json.Marshal(owner)
			if err != nil {
				t.Fatal(err)
			}
			if err := journal.WriteAtomic(feed.pluginPath(), raw); err != nil {
				t.Fatal(err)
			}
			if _, err := feed.PluginStatus(); !errors.Is(err, ErrOldPluginRecord) {
				t.Fatal("F_OWNER_LEGACY_CURSOR_REINSTALL_REQUIRED", err)
			}
			if !bytes.Equal(before, checkpointImage(t, feed)) || !bytes.Equal(raw, readCommitFile(t, feed.pluginPath())) {
				t.Fatal("F_OWNER_LEGACY_CURSOR_NO_MIGRATION")
			}
		})
	}
}

func TestStreamCheckpointMovementDoesNotChangeOwnerBinding(t *testing.T) {
	feed, launch, _, _, offer := streamPendingMessageFixture(t)
	owner := readCommitFile(t, feed.pluginPath())
	before := checkpointImage(t, feed)
	streamComplete(t, feed, launch, offer)
	after, err := feed.PluginStatus()
	if err != nil || after.Revision != launch.Revision || after.Epoch != launch.Epoch || !bytes.Equal(owner, readCommitFile(t, feed.pluginPath())) || bytes.Equal(before, checkpointImage(t, feed)) {
		t.Fatal("F_CHECKPOINT_MOVEMENT_EXCLUDED_FROM_OWNER_BINDING", err)
	}
}
