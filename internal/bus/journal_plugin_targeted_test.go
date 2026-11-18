package bus

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/zalkilabs/zalki/internal/journal"
)

type memberWorkCount struct{ input, reads, native, tokens, probes, limit int }

func (c *memberWorkCount) charge(input, reads, native, tokens, probes int) error {
	if c.limit > 0 && c.input+input > c.limit {
		return errors.New("controlled work limit")
	}
	c.input += input
	c.reads += reads
	c.native += native
	c.tokens += tokens
	c.probes += probes
	return nil
}

func collectMembers(t *testing.T, store Store, targets []string, count *memberWorkCount) (pluginMemberCollection, error) {
	t.Helper()
	var result pluginMemberCollection
	err := WithJournalWrite(store, func(view JournalView) error {
		raw, err := PluginMembers(t.Context(), view, "first", "all", true, targets, count.charge)
		if err != nil {
			return err
		}
		return json.Unmarshal(raw, &result)
	})
	return result, err
}

func TestFeedTargetedProjectionHasNoImplicitCensus(t *testing.T) {
	store, session, _, _ := memberCacheFixture(t)
	other, _ := register(t, store, "fixture")
	assertTargetedEmptyAndNil(t, store)
	path, err := store.sessionPath(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad-json"), 0600); err != nil {
		t.Fatal(err)
	}
	one := &memberWorkCount{}
	result, err := collectMembers(t, store, []string{session.ID}, one)
	if err != nil || len(result.Records) != 1 || result.Records[0].ID != session.ID || result.Records[0].Class != "active" || one.reads != 1 || one.native != 1 || one.probes != 1 {
		t.Fatal("B_TARGETED_ONLY_CURRENT_NATIVE_RECORD", one, err)
	}
}

func assertTargetedEmptyAndNil(t *testing.T, store Store) {
	t.Helper()
	empty := &memberWorkCount{}
	result, err := collectMembers(t, store, []string{}, empty)
	if err != nil || len(result.Records) != 0 || empty.reads != 0 || empty.native != 0 || empty.probes != 0 {
		t.Fatal("B_TARGETED_EMPTY_READS_NO_NATIVE", empty, err)
	}
	nilWork := &memberWorkCount{}
	if _, err := collectMembers(t, store, nil, nilWork); !errors.Is(err, journal.ErrInvalidFile) || nilWork.reads != 0 || nilWork.native != 0 {
		t.Fatal("B_TARGETED_NIL_CANNOT_START_CENSUS", nilWork, err)
	}
}

func TestFeedTargetedBudgetRefusesBeforeReadAndProbe(t *testing.T) {
	store, session, _, _ := memberCacheFixture(t)
	count := &memberWorkCount{limit: 1}
	if _, err := collectMembers(t, store, []string{session.ID}, count); err == nil || count.reads != 0 || count.probes != 0 {
		t.Fatal("B_TARGETED_SHARED_BUDGET_BEFORE_NATIVE_ALLOCATION", count, err)
	}
}
