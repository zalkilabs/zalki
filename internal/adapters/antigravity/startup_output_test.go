package antigravity

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

// The startup output is the system message Antigravity gives the new conversation.
// The bytes are fixed on purpose: agy 1.5.18 accepted exactly this shape in a live
// run, so a change of a key or of the escaping must be a decision. Core adds one
// newline after the JSON of the adapter's value, as the test does.

func startupFixture() agent.StartupContext {
	return agent.StartupContext{
		Actor: "antigravity:00000000-0000-4000-8000-0000000000d4", Capability: "fixture-capability",
		StateDir: "/fixture/state dir", ConfigFile: "/fixture/config.json", Executable: "/fixture/bin/zalki's",
		Instructions: "Zalki actor: A. Fixture <b> & \"quoted\" text, caf\u00e9, line\nbreak, U+2028 \u2028 end.",
	}
}

func TestStartupOutputIsExactlyTheGoldenFile(t *testing.T) {
	out, err := New().StartupOutput(startupFixture())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "startup-output.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("startup output differs from testdata/startup-output.jsonl\ngot:  %q\nwant: %q", raw, want)
	}
}
