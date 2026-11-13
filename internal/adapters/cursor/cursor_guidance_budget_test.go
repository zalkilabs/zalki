package cursor

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
)

func TestCursorRecoveryGuidanceBoundsFullJSON(t *testing.T) {
	for _, field := range []string{"additional_context", "followup_message"} {
		empty, err := json.Marshal(map[string]string{field: ""})
		if err != nil {
			t.Fatal(err)
		}
		limitText := strings.Repeat("x", 24*1024-len(empty)-1)
		for _, text := range []string{limitText, limitText + "x", strings.Repeat("\x00", 4500)} {
			assertCursorGuidanceBudget(t, field, text)
		}
	}
}

func TestCursorRecoveryGuidanceQuoteHeavyPathLimits(t *testing.T) {
	for _, length := range []int{1024, 4096} {
		path := quoteHeavyCursorPath(length)
		grant := agent.RecoveryGrant{Actor: "cursor:00000000-0000-4000-8000-000000000001", Capability: "fixture-private-capability", StateDir: path, ConfigFile: path, Instructions: "Private authority and Boundary instructions."}
		text := cursorRecoveryContext(path, grant)
		if !strings.Contains(text, grant.Capability) || !strings.Contains(text, cursorListenerCommand(path, grant.Actor, grant.Capability, path, path)) {
			t.Fatal("C19_GUIDANCE_COMMAND_AND_KEY_NEVER_TRUNCATED")
		}
		assertCursorGuidanceBudget(t, "additional_context", text)
	}
}

func quoteHeavyCursorPath(length int) string {
	var path strings.Builder
	for path.Len() < length {
		path.WriteByte('/')
		path.WriteString(strings.Repeat("'", min(120, length-path.Len())))
	}
	return path.String()
}

func assertCursorGuidanceBudget(t *testing.T, field, text string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{field: text})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	stderr := cursorGuidanceStderr(t, func() {
		if err := writeCursorHookOutput(&output, field, text, true); err != nil {
			t.Fatal(err)
		}
	})
	if len(text) > 24*1024 || len(raw)+1 > 24*1024 {
		if output.Len() != 0 || !strings.Contains(stderr, "Shorten the configured paths, then start a new chat.") || strings.Contains(stderr, "fixture-private-capability") {
			t.Fatal("C19_OVERSIZE_GUIDANCE_DECLINES_WITHOUT_PARTIAL_SECRET_OUTPUT")
		}
		return
	}
	if stderr != "" || output.String() != string(raw)+"\n" || output.Len() > 24*1024 {
		t.Fatal("C19_COMPLETE_GUIDANCE_FITS_EXACT_JSON_BUDGET")
	}
}

func cursorGuidanceStderr(t *testing.T, run func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = original; _ = reader.Close(); _ = writer.Close() }()
	run()
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}
