package cursor

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func TestListenerGuidancePreservesLiteralArguments(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(t.TempDir(), "settings ' $word `printf altered`.json")
	state := filepath.Join(t.TempDir(), "state ' $(printf altered)")
	const actor, key = "cursor:61000000-0000-4000-8000-000000000001", "fixture ' $word `printf altered`"
	startup, err := (Adapter{}).StartupOutput(agent.StartupContext{Actor: actor, Capability: key, StateDir: state, ConfigFile: configFile})
	if err != nil {
		t.Fatal(err)
	}
	contexts := []string{startup.(map[string]any)["additional_context"].(string), cursorRecoveryContext(binary, agent.RecoveryGrant{Actor: actor, Capability: key, StateDir: state, ConfigFile: configFile})}
	for _, context := range contexts {
		start := strings.Index(context, "ZALKI_CAPABILITY=")
		if start < 0 {
			t.Fatal("listener command missing")
		}
		end := strings.LastIndex(context[start:], "`.")
		if end < 0 {
			t.Fatal("listener command terminator missing")
		}
		command := context[start : start+end]
		// Parse the command into argv without invoking its executable.
		out, err := exec.CommandContext(t.Context(), "/bin/sh", "-c", "set -- "+command+"\nprintf '%s\\000' \"$@\"").Output()
		got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
		want := []string{"ZALKI_CAPABILITY=" + key, binary, "--config", configFile, "--state", state, "cursor-listen", "--actor", actor}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("listener guidance altered literal arguments", err)
		}
	}
}
