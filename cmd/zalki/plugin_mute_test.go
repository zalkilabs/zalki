package main

import (
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

func TestPluginMuteCobraSelfOnly(t *testing.T) {
	store, _ := pluginOwnerFixture(t)
	session, key, err := store.Register("cursor", "mute-self", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZALKI_ACTOR", session.ID)
	t.Setenv("ZALKI_CAPABILITY", key)
	raw, err := pluginOwnerLocal(t, store.Dir, "mute", "external")
	if err != nil || string(raw) != "Muted external for this session.\n" {
		t.Fatal("Z6_COBRA_MUTE_EXECUTES", string(raw), err)
	}
	var member bool
	if err := bus.WithJournalRead(store, func(v bus.JournalView) error {
		var err error
		member, err = v.PluginMember(session.ID, "external")
		return err
	}); err != nil || member {
		t.Fatal("Z6_COBRA_MUTATION_PERSISTED", err)
	}
	for _, flag := range []string{"--session", "--session-id", "--capability", "--key"} {
		if _, err := pluginOwnerLocal(t, store.Dir, "unmute", "external", flag, session.ID); err == nil {
			t.Fatal("Z6_COBRA_REJECTS_SELECTOR_AND_KEY_FLAGS", flag)
		}
	}
	raw, err = pluginOwnerLocal(t, store.Dir, "unmute", "external")
	if err != nil || string(raw) != "Unmuted external for this session. Owner and ZALKI_PLUGINS restrictions still apply.\n" {
		t.Fatal("Z6_COBRA_UNMUTE_EXECUTES", string(raw), err)
	}
}

func TestPluginMuteCLIAuthAndHelp(t *testing.T) {
	store, other := pluginOwnerFixture(t)
	session, key, err := store.Register("cursor", "mute-auth", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZALKI_ACTOR", session.ID)
	t.Setenv("ZALKI_CAPABILITY", key)
	raw, err := pluginOwnerLocal(t, store.Dir, "mute", "external", "--actor", other.ID)
	if err == nil || strings.Contains(string(raw), key) {
		t.Fatal("Z6_CLI_CROSS_ACTOR_AUTH_CLOSED")
	}
	t.Setenv("ZALKI_CAPABILITY", "")
	if _, err := pluginOwnerLocal(t, store.Dir, "mute", "external"); err == nil {
		t.Fatal("Z6_CLI_KEY_REQUIRED")
	}
	raw, err = pluginOwnerLocal(t, store.Dir, "mute", "--help")
	if err != nil || !strings.Contains(string(raw), "authorized access") || strings.Contains(string(raw), "--session ") {
		t.Fatal("Z6_CLI_HELP_SCOPE", string(raw), err)
	}
}
