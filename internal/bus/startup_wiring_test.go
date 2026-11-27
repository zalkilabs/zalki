package bus

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// recordedStartupHook returns a fixed value and records what core asked it to render.
type recordedStartupHook struct {
	agent.StandardHooks
	value any
	asked *[]agent.StartupContext
}

func (h recordedStartupHook) StartupOutput(v agent.StartupContext) (any, error) {
	*h.asked = append(*h.asked, v)
	return h.value, nil
}

func startupWithPluginText(t *testing.T, s Store, value any, text string) ([]byte, []agent.StartupContext, model.Session, string) {
	t.Helper()
	current, key := register(t, s, "cursor")
	var asked []agent.StartupContext
	service := Service{Store: s, PluginStartup: func(_ JournalView, _ string, _ model.Session, _ bool, _ func(string) (bool, error)) (string, error) {
		return text, nil
	}}
	var out bytes.Buffer
	if err := service.sessionStartupOutput(recordedStartupHook{value: value, asked: &asked}, current, key, true, &out); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), asked, current, key
}

func TestCoreWritesTheJSONOfTheAdaptersValueAndOneNewline(t *testing.T) {
	got, _, _, _ := startupWithPluginText(t, fixtureStore(t), map[string]any{"b": "x < y & z", "a": []string{"é"}}, "plugins: none")
	if want := `{"a":["é"],"b":"x \u003c y \u0026 z"}` + "\n"; string(got) != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestCoreAsksTheAdapterForTheBaseTextAndThenForTheTextWithPlugins(t *testing.T) {
	s := fixtureStore(t)
	s.configFile = "/fixture/config.json"
	_, asked, current, key := startupWithPluginText(t, s, "fixed", "plugins: none")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 2 {
		t.Fatalf("adapter was asked %d times, want 2 (base, then with the plugin text)", len(asked))
	}
	base := startupInstructions(current.ID, key)
	for i, want := range []string{base, base + "\nplugins: none"} {
		got := asked[i]
		if got.Actor != current.ID || got.Capability != key || got.StateDir != s.dir || got.ConfigFile != s.configFile || got.Executable != binary || got.Instructions != want {
			t.Fatalf("ask %d = %+v, want actor %s, state %s, executable %s and instructions %q", i, got, current.ID, s.dir, binary, want)
		}
	}
	if !strings.HasPrefix(asked[1].Instructions, asked[0].Instructions) {
		t.Fatal("the second ask must extend the first")
	}
}
