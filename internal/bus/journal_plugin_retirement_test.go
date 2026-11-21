package bus

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
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
	discardPluginRetirement(store.dir, session.ID)
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
	store, session, original := ownerTrackingMemberFixture(t)
	before, err := collectMembers(t, store, []string{session.ID}, &memberWorkCount{})
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range []*model.ProcessRef{{PID: 800002, Start: "owner-b", Role: "client"}, &original} {
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

// Owner ABA needs an adapter that permits stored owners. Publish A through the
// normal audited writer and capture it through the same decoder used by every
// later read; changing the registry after registration would hide this premise.
func ownerTrackingMemberFixture(t *testing.T) (Store, model.Session, model.ProcessRef) {
	t.Helper()
	registry, err := agent.NewRegistry(livenessStub{result: model.LivenessAlive, own: true})
	if err != nil {
		t.Fatal(err)
	}
	store := fixtureStore(t)
	store.registry = registry
	session, _ := register(t, store, "fixture")
	original := model.ProcessRef{PID: 800001, Start: "owner-a", Role: "client"}
	if err := store.locked(func() error {
		current, err := store.session(session.ID)
		if err != nil {
			return err
		}
		current.Owner = &original
		return saveSessionFor(store, current, "")
	}); err != nil {
		t.Fatal("owner-tracking fixture could not publish original owner", err)
	}
	session, err = store.session(session.ID)
	if err != nil || !samePluginOwner(session.Owner, &original) {
		t.Fatal("owner-tracking fixture did not retain the captured original", err)
	}
	return store, session, original
}
