package bus

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSetTaskStoresClearsAndRejectsLimits(t *testing.T) {
	store := fixtureStore(t)
	session, key, other, otherKey := registerPair(t, store, "cursor", "codex")
	service := &Service{Store: store}

	stored, err := service.SetTask(session.ID, key, "review the queue")
	if err != nil || stored.Task != "review the queue" {
		t.Fatalf("set: %+v %v", stored, err)
	}
	current, err := store.session(session.ID)
	if err != nil || current.Task != "review the queue" || current.UpdatedAt != session.UpdatedAt {
		t.Fatalf("claim was not stored on the caller: %+v %v", current, err)
	}
	if _, err := service.SetTask(other.ID, key, "not yours"); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("another agent's capability was accepted", err)
	}
	untouched, err := store.session(other.ID)
	if err != nil || untouched.Task != "" {
		t.Fatalf("another agent's task changed: %+v %v", untouched, err)
	}
	if _, err := service.SetTask(session.ID, otherKey, "review the queue"); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("wrong capability was accepted", err)
	}

	exact := strings.Repeat("你", 40) // 120 bytes
	if _, err := service.SetTask(session.ID, key, exact); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{
		strings.Repeat("你", 41),
		strings.Repeat("a", taskLimit+1),
		"two\nlines",
		"tab\there",
		" padded",
		"padded ",
		" ",
		string([]byte{0xff}),
		"nul\x00",
	} {
		if _, err := service.SetTask(session.ID, key, invalid); ErrorCodeOf(err) != CodeInvalidInput {
			t.Fatalf("accepted %q: %v", invalid, err)
		}
	}
	kept, err := store.session(session.ID)
	if err != nil || kept.Task != exact {
		t.Fatalf("a rejected task replaced the stored claim: %+v %v", kept.Task, err)
	}
	cleared, err := service.SetTask(session.ID, key, "")
	if err != nil || cleared.Task != "" {
		t.Fatalf("clear: %+v %v", cleared, err)
	}
	if current, err = store.session(session.ID); err != nil || current.Task != "" {
		t.Fatalf("empty task did not clear the claim: %+v %v", current, err)
	}
}

func TestTaskIsListedOnlyWhenSetAndIsNotAnAddress(t *testing.T) {
	store := fixtureStore(t)
	sender, senderKey, named, _ := registerPair(t, store, "cursor", "codex")
	claimant, claimantKey := register(t, store, "cursor")
	if err := store.hookName(named.ID, "beta"); err != nil {
		t.Fatal(err)
	}
	service := &Service{Store: store}
	if _, err := service.SetTask(claimant.ID, claimantKey, "beta"); err != nil {
		t.Fatal(err)
	}
	listed, err := store.Discover()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, agent := range listed {
		seen[agent.ID] = agent.Task
		encoded, err := json.Marshal(agent)
		if err != nil {
			t.Fatal(err)
		}
		if agent.Task == "" && strings.Contains(string(encoded), `"task"`) {
			t.Fatalf("empty task was listed: %s", encoded)
		}
	}
	if seen[claimant.ID] != "beta" || seen[named.ID] != "" || seen[sender.ID] != "" {
		t.Fatalf("list showed the wrong claims: %+v", seen)
	}
	target, err := store.resolve("beta")
	if err != nil || target.ID != named.ID {
		t.Fatalf("task was used as an address: %+v %v", target, err)
	}
	message, _, _, err := store.ReserveMessage(SendRequest{
		Actor: sender.ID, Capability: senderKey, To: "beta", MessageID: newID(), Body: "ping",
	})
	if err != nil || message.To != named.ID {
		t.Fatalf("send followed the task claim: %+v %v", message, err)
	}
}

func TestTaskSurvivesResumeAndClearsOnRetirement(t *testing.T) {
	store := fixtureStore(t)
	session, key := register(t, store, "cursor")
	service := &Service{Store: store}
	if _, err := service.SetTask(session.ID, key, "keep across resume"); err != nil {
		t.Fatal(err)
	}
	held, err := store.session(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	held.ApprovalHeld = 2
	if err := store.locked(func() error { return store.saveSession(held) }); err != nil {
		t.Fatal(err)
	}
	resumed, resumeKey, err := store.register(session.Kind, session.NativeID, session.CWD, nil, true)
	if err != nil || resumed.Generation != session.Generation || resumed.Task != "keep across resume" || resumed.ApprovalHeld != 2 {
		t.Fatalf("resume dropped the claim or approval count: %+v %v", resumed, err)
	}
	if _, err := service.SetTask(session.ID, key, "stale key"); ErrorCodeOf(err) != CodeUnauthorized {
		t.Fatal("resume kept the old capability", err)
	}
	if err := store.Update(session.ID, StateRetired, true); err != nil {
		t.Fatal(err)
	}
	retired, err := store.session(session.ID)
	if err != nil || !retired.Retired || retired.Task != "" {
		t.Fatalf("retirement kept the claim: %+v %v", retired, err)
	}
	replaced, _, err := store.Register(session.Kind, session.NativeID, session.CWD, nil)
	if err != nil || replaced.Generation == session.Generation || replaced.Task != "" {
		t.Fatalf("a new registration reused the retired claim: %+v %v", replaced, err)
	}
	_ = resumeKey

	explicit, explicitKey := register(t, store, "codex")
	if _, err := service.SetTask(explicit.ID, explicitKey, "prune me"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Prune(false, []string{explicit.ID}); err != nil {
		t.Fatal(err)
	}
	pruned, err := store.session(explicit.ID)
	if err != nil || !pruned.Retired || pruned.Task != "" {
		t.Fatalf("prune kept the claim: %+v %v", pruned, err)
	}
}
