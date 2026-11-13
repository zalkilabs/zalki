package bus

import (
	"encoding/json"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func assertSessionPluginPublication(t *testing.T, record map[string]any, env string) {
	t.Helper()
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var session model.Session
	if err := json.Unmarshal(raw, &session); err != nil {
		t.Fatal(err)
	}
	p, valid := decodeSessionPluginPolicy(session)
	want := parseSessionPluginEnvironment(env)
	if !valid || p.Mode != want.Mode || !slices.Equal(p.Allow, want.Allow) || p.InvalidEnv != want.InvalidEnv || p.LastChange.Plugin != "" || p.LastChange.Muted != nil {
		t.Fatal("Z6_FIRST_PUBLICATION_POLICY_AND_BASELINE_AUDIT")
	}
}

func TestSessionPluginRealHookEnvironmentAuthority(t *testing.T) {
	t.Setenv("ZALKI_PLUGINS", "off")
	for _, event := range []string{"sessionStart", "beforeSubmitPrompt"} {
		s := fixtureStore(t)
		native := newID()
		input := `{"conversation_id":"` + native + `","session_id":"` + native + `","cwd":"/workspace","hook_event_name":"` + event + `","prompt":"owner","plugin_policy":{"mode":"owner"},"ZALKI_PLUGINS":"external"}`
		if err := (&Service{Store: s}).Hook(t.Context(), "cursor", strings.NewReader(input), io.Discard); err != nil {
			t.Fatal(err)
		}
		if pluginMemberForTest(t, s, "cursor:"+native, "external") {
			t.Fatal("Z6_HOOK_JSON_CANNOT_OVERRIDE_ENV")
		}
	}
}
