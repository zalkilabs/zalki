package bus

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

func trustedPrompt(native string) agent.HookInput {
	return agent.HookInput{SessionID: native, CWD: "/workspace", Event: "UserPromptSubmit", AllowPromptRegistration: true}
}

func TestPromptRegistrationRequiresTrustedCanonicalPrompt(t *testing.T) {
	for _, test := range []struct {
		event   string
		trusted bool
	}{
		{"PostToolUse", true}, {"Stop", true}, {"SessionEnd", true}, {"UserPromptSubmit", false},
	} {
		store := fixtureStore(t)
		input := trustedPrompt(newID())
		input.Event, input.AllowPromptRegistration = test.event, test.trusted
		actor, err := store.sessionID("cursor", input.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := prepareHookRuntime(store, "cursor", actor, input); !errors.Is(err, errStaleHook) {
			t.Fatal("C01_ONLY_TRUSTED_PROMPT_CREATES", test, err)
		}
		if _, err := store.session(actor); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("C01_NO_RECORD_FROM_OTHER_EVENTS", err)
		}
	}
	var forged agent.HookInput
	if err := json.Unmarshal([]byte(`{"AllowPromptRegistration":true,"allow_prompt_registration":true,"hook_event_name":"UserPromptSubmit"}`), &forged); err != nil || forged.AllowPromptRegistration {
		t.Fatal("C01_JSON_CANNOT_SELECT_REGISTRATION", err)
	}
}

func TestPromptRegistrationBindsCanonicalActor(t *testing.T) {
	store := fixtureStore(t)
	event := trustedPrompt(newID())
	if _, err := prepareHookRuntime(store, "cursor", "cursor:"+newID(), event); model.ErrorCodeOf(err) != model.CodeInvalidIdentity {
		t.Fatal("C01_CANONICAL_ACTOR_BINDING_REQUIRED", err)
	}
}

func TestPromptRegistrationInvalidTitleMatchesStartup(t *testing.T) {
	store := fixtureStore(t)
	event := trustedPrompt(newID())
	title := "invalid\ntitle"
	event.SessionTitle = &title
	actor := "cursor:" + event.SessionID
	if _, err := prepareHookRuntime(store, "cursor", actor, event); err != nil {
		t.Fatal("C02_INVALID_TITLE_IS_IGNORED", err)
	}
	v, err := store.session(actor)
	if err != nil || v.Name != "" {
		t.Fatal("C02_INVALID_TITLE_IS_IGNORED", err)
	}
}

func TestPromptRegistrationPreservesExistingAndRetired(t *testing.T) {
	store := fixtureStore(t)
	v, key := register(t, store, "cursor")
	if err := store.hookName(v.ID, "chosen name"); err != nil {
		t.Fatal(err)
	}
	for _, retired := range []bool{false, true} {
		if retired {
			if err := store.Update(v.ID, model.StateRetired, true); err != nil {
				t.Fatal(err)
			}
		}
		host, err := prepareHookRuntime(store, "cursor", v.ID, trustedPrompt(v.NativeID))
		if err != nil {
			t.Fatal(err)
		}
		if err := host.ApplyLifecycle(stubResumeOnPrompt); err != nil {
			t.Fatal(err)
		}
		current, err := store.authenticate(v.ID, key)
		if err != nil || current.Generation != v.Generation || current.Name != "chosen name" || current.Retired {
			t.Fatal("C04_EXISTING_PROMPT_NEVER_REPLACES", err, current)
		}
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "hook-registration-quota.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("C04_EXISTING_RECORD_NOT_CHARGED", err)
	}
}

func TestPromptRegistrationRejectsDamagedExistingRecord(t *testing.T) {
	for _, damage := range []string{"invalid", "unsupported", "symlink"} {
		t.Run(damage, func(t *testing.T) { assertDamagedPromptRecord(t, damage) })
	}
}

func assertDamagedPromptRecord(t *testing.T, damage string) {
	t.Helper()
	store := fixtureStore(t)
	v, _ := register(t, store, "cursor")
	path, err := store.sessionPath(v.ID)
	if err != nil {
		t.Fatal(err)
	}
	if damage == "symlink" {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
			t.Fatal(err)
		}
	} else {
		raw := []byte("{broken")
		if damage == "unsupported" {
			var record map[string]any
			if err := json.Unmarshal(sessionBytes(t, store, v.ID), &record); err != nil {
				t.Fatal(err)
			}
			record["schema"] = 999
			raw, err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := prepareHookRuntime(store, "cursor", v.ID, trustedPrompt(v.NativeID)); err == nil {
		t.Fatal("C03_ONLY_ENOENT_IS_ABSENCE")
	}
	if _, err := os.Stat(filepath.Join(store.Dir, "hook-registration-quota.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("C03_DAMAGED_RECORD_NOT_CHARGED", err)
	}
}

func TestPromptRegistrationDurableDebitPrecedesWAL(t *testing.T) {
	for _, failAfterRename := range []bool{false, true} {
		assertPromptDebitFailure(t, failAfterRename)
	}
}

func assertPromptDebitFailure(t *testing.T, failAfterRename bool) {
	t.Helper()
	store := fixtureStore(t)
	event := trustedPrompt(newID())
	actor, err := store.sessionID("cursor", event.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	err = store.locked(func() error {
		_, err := registerPromptAbsent(store, "cursor", actor, event, now, func(path string, raw []byte) error {
			if _, err := store.session(actor); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("C08_DEBIT_BEFORE_SESSION", err)
			}
			if failAfterRename {
				if err := journal.WriteAtomic(path, raw); err != nil {
					return err
				}
			}
			return errors.New("simulated quota durability failure")
		})
		return err
	})
	if err == nil {
		t.Fatal("C08_DEBIT_FAILURE_PROPAGATES")
	}
	if _, err := store.session(actor); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("C08_NO_SESSION_AFTER_FAILED_DEBIT", err)
	}
	if failAfterRename {
		if err := debitPromptRegistration(store.Dir, actor, now.Add(time.Second), journal.WriteAtomic); !errors.Is(err, errPromptQuota) {
			t.Fatal("C08_PUBLISHED_FAILED_DEBIT_IS_SPENT", err)
		}
	}
}

func TestPromptRegistrationSharedInitialization(t *testing.T) {
	for _, policy := range []string{"", "off", "one,two", "invalid setting"} {
		t.Run(policy, func(t *testing.T) { assertPromptInitializationParity(t, policy) })
	}
}

func TestPromptRegistrationSecretsStayPrivate(t *testing.T) {
	store := fixtureStore(t)
	event := trustedPrompt(newID())
	actor, err := store.sessionID("cursor", event.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	host, err := prepareHookRuntime(store, "cursor", actor, event)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := finishPromptHook(store, &output, nil); err != nil || strings.TrimSpace(output.String()) != `{"continue":true}` {
		t.Fatal("C10_PROMPT_HAS_NO_CAPABILITY", err, output.String())
	}
	host.event = "PostToolUse"
	grant, err := host.ClaimRecovery(stubClaimRecovery)
	if err != nil || grant.Capability == "" || !strings.Contains(grant.Instructions, grant.Capability) {
		t.Fatal("C10_PRIVATE_RECOVERY_HAS_AUTHORITY", err)
	}
	raw, err := json.Marshal(grant)
	if err != nil || bytes.Contains(raw, []byte(grant.Capability)) || bytes.Contains(raw, []byte(grant.Instructions)) {
		t.Fatal("C10_GRANT_JSON_EXCLUDES_SECRETS", err)
	}
	secret := regexp.MustCompile(`[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}`)
	for path, raw := range journalSnapshot(t, store) {
		if secret.MatchString(raw) || strings.Contains(raw, grant.Capability) {
			t.Fatal("C10_NO_PLAINTEXT_AT_REST", path)
		}
	}
}

func TestPromptRegistrationNativeIdentityAndTitle(t *testing.T) {
	store := fixtureStore(t)
	native := newID()
	title := "Resumed chat"
	event := trustedPrompt(strings.ToUpper(native))
	event.SessionTitle = &title
	actor, err := store.sessionID("cursor", event.SessionID)
	if err != nil || actor != "cursor:"+native {
		t.Fatal("C02_CANONICAL_ACTOR", actor, err)
	}
	if _, err := prepareHookRuntime(store, "cursor", actor, event); err != nil {
		t.Fatal("C17_CANONICAL_ALIAS_REGISTRATION", err)
	}
	v, err := store.session(actor)
	if err != nil || v.NativeID != native || v.Name != title || v.CWD != "/workspace" {
		t.Fatal("C02_PROMPT_ID_CWD_TITLE", err, v)
	}
	if _, err := store.PruneRetired(false, []string{actor}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneRetired(false, nil, 0); err != nil {
		t.Fatal(err)
	}
	event.SessionID = native
	if _, err := prepareHookRuntime(store, "cursor", actor, event); !errors.Is(err, errPromptQuota) {
		t.Fatal("C17_ALIASES_SHARE_QUOTA", err)
	}
}

func TestPromptRegistrationRejectsNativeCollision(t *testing.T) {
	store := fixtureStore(t)
	v, key, err := store.Register("cursor", "a non UUID native conversation", "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	before := sessionBytes(t, store, v.ID)
	event := trustedPrompt(strings.TrimPrefix(v.ID, "cursor:"))
	if _, err := prepareHookRuntime(store, "cursor", v.ID, event); model.ErrorCodeOf(err) != model.CodeConflict {
		t.Fatal("C17_NATIVE_ID_COLLISION_REFUSED", err)
	}
	if !bytes.Equal(before, sessionBytes(t, store, v.ID)) {
		t.Fatal("C17_COLLISION_LEFT_RECORD_UNCHANGED")
	}
	if _, err := store.authenticate(v.ID, key); err != nil {
		t.Fatal(err)
	}
}

func TestPromptRegistrationPruneFencesAutomaticDeliveryButKeepsHistory(t *testing.T) {
	store := fixtureStore(t)
	from, key, old, oldKey := registerPair(t, store, "codex", "cursor")
	service := Service{Store: store}
	first, err := service.Send(t.Context(), SendRequest{Actor: from.ID, Capability: key, To: old.ID, MessageID: newID(), Body: "OLD_GENERATION_BODY"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneRetired(false, []string{old.ID}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PruneRetired(false, nil, 0); err != nil {
		t.Fatal(err)
	}
	host, err := prepareHookRuntime(store, "cursor", old.ID, trustedPrompt(old.NativeID))
	if err != nil {
		t.Fatal(err)
	}
	current, err := store.session(old.ID)
	if err != nil || current.Generation == old.Generation || current.KeyHash == old.KeyHash {
		t.Fatal("C14_PRUNE_GETS_FRESH_GENERATION_AND_KEY", err)
	}
	if _, err := store.authenticate(old.ID, oldKey); err == nil {
		t.Fatal("C14_OLD_KEY_REJECTED")
	}
	host.event = "PostToolUse"
	grant, err := host.ClaimRecovery(stubClaimRecovery)
	if err != nil || grant.Capability == "" {
		t.Fatal(err)
	}
	text, err := host.EmitPending(old.ID)
	if err != nil || strings.Contains(text, "OLD_GENERATION_BODY") {
		t.Fatal("C14_OLD_AUTO_DELIVERY_FENCED", err)
	}
	assertPromptActorHistory(t, store, old.ID, grant.Capability, first.ID)
}

func assertPromptActorHistory(t *testing.T, store Store, actor, key, parent string) {
	t.Helper()
	items, err := store.Messages(actor, key)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.ID == parent {
			found = true
		}
	}
	if !found {
		t.Fatal("C20_HISTORY_REMAINS_ACTOR_BASED")
	}
}
