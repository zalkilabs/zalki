package bus

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestFeedMemberRetirementWitnessComesOnlyFromDirectPublishedWrite(t *testing.T) {
	store, session, _, _ := memberCacheFixture(t)
	before, err := collectMembers(t, store, []string{session.ID}, &memberWorkCount{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Update(session.ID, model.StateRetired, true); err != nil {
		t.Fatal(err)
	}
	work := &memberWorkCount{}
	after, err := collectMembers(t, store, []string{session.ID}, work)
	if err != nil || len(after.Records) != 1 {
		t.Fatal(err)
	}
	row := after.Records[0]
	if row.Class != "absent" || row.Retirement == nil || row.Retirement.Before != before.Records[0].Guard || row.Retirement.After != row.Guard || row.Retirement.Digest != row.Digest || !model.IsCanonicalUUID(row.Retirement.Event) {
		t.Fatal("F_RETIREMENT_EXACT_SAME_WRITE_PROOF_POSITIVE")
	}
	if work.native != 1 || work.probes != 0 {
		t.Fatal("F_RETIREMENT_PROJECTION_NO_ADDITIONAL_NATIVE_PROBE", work)
	}
	discardPluginRetirement(store.Dir, session.ID)
	lost, err := collectMembers(t, store, []string{session.ID}, &memberWorkCount{})
	if err != nil || lost.Records[0].Retirement != nil {
		t.Fatal("F_RETIREMENT_PROOF_LOSS_IS_CONSERVATIVE", err)
	}
}

func TestFeedMemberOwnerExitedNeverProducesRetirementWitness(t *testing.T) {
	store, session, _, _ := memberCacheFixture(t)
	if err := updateSession(store, session.ID, model.StateRetired, true, model.CauseOwnerExited); err != nil {
		t.Fatal(err)
	}
	rows, err := collectMembers(t, store, []string{session.ID}, &memberWorkCount{})
	if err != nil || len(rows.Records) != 1 || rows.Records[0].Retirement != nil {
		t.Fatal("F_RETIREMENT_OWNER_EXIT_SOURCE_NEVER_ATTESTED", err)
	}
}

func TestFeedMemberOwnerABACannotReuseOriginalGuard(t *testing.T) {
	store, session, _, _ := memberCacheFixture(t)
	before, err := collectMembers(t, store, []string{session.ID}, &memberWorkCount{})
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []*model.ProcessRef{{PID: 800001, Start: "owner-a", Role: "client"}, nil} {
		if err := store.locked(func() error {
			current, err := store.session(session.ID)
			if err != nil {
				return err
			}
			current.Owner = owner
			return saveSessionFor(store, current, "")
		}); err != nil {
			t.Fatal(err)
		}
	}
	after, err := collectMembers(t, store, []string{session.ID}, &memberWorkCount{})
	if err != nil || after.Records[0].Class != "active" || before.Records[0].Guard.Owner != after.Records[0].Guard.Owner || before.Records[0].Guard.Audit == after.Records[0].Guard.Audit {
		t.Fatal("F_NATIVE_OWNER_ABA_CANNOT_REUSE_ORIGINAL_GUARD", err)
	}
}
