package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// timeBus returns stored UTC times, as the store holds them.
type timeBus struct{ *fakeBus }

func (timeBus) DiscoverWithWarnings(string, string) ([]bus.AgentInfo, int, error) {
	return []bus.AgentInfo{{ID: "codex:" + fakeMessageID, UpdatedAt: "2026-11-22T17:35:23Z"}}, 0, nil
}

func (timeBus) ReadMessages(bus.MessageQuery) (bus.MessageBatch, error) {
	return bus.MessageBatch{Messages: []model.Message{{
		ID: fakeMessageID, To: "claude:" + fakeMessageID, Body: "text", Status: model.StatusSubmitted,
		CreatedAt: "2026-11-22T17:35:23.123456789Z", AcknowledgedAt: "2026-11-22T17:40:00Z",
	}}}, nil
}

func (timeBus) Watch(_, _, to string) (bus.WatchResult, error) {
	return bus.WatchResult{Status: "armed", To: to, At: "2026-11-22T17:35:23Z", ExpiresAt: "2026-11-23T05:35:23Z"}, nil
}

func TestMCPOutputShowsLocalTimeWithOffset(t *testing.T) {
	client, ctx := fakeClient(t, timeBus{&fakeBus{}})
	call := func(tool string) map[string]any {
		t.Helper()
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"actor": "a", "capability": "k"}})
		if err != nil || result.IsError {
			t.Fatalf("%s: %+v %v", tool, result, err)
		}
		encoded, _ := json.Marshal(result.StructuredContent)
		var out map[string]any
		if err := json.Unmarshal(encoded, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	agents := call("list_agents")["data"].([]any)[0].(map[string]any)
	if agents["updated_at"] != "2026-11-22T19:35:23+02:00" {
		t.Fatalf("list_agents updated_at = %v", agents["updated_at"])
	}
	watch, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "watch_agent", Arguments: map[string]any{"actor": "a", "capability": "k", "to": "t"}})
	if err != nil || watch.IsError {
		t.Fatalf("watch_agent: %+v %v", watch, err)
	}
	encodedWatch, _ := json.Marshal(watch.StructuredContent)
	var watched struct {
		Data struct {
			At        string `json:"at"`
			ExpiresAt string `json:"expires_at"`
		} `json:"data"`
	}
	if err := json.Unmarshal(encodedWatch, &watched); err != nil || watched.Data.At != "2026-11-22T19:35:23+02:00" || watched.Data.ExpiresAt != "2026-11-23T07:35:23+02:00" {
		t.Fatalf("watch_agent times = %+v %v", watched.Data, err)
	}
	page := call("list_messages")["data"].(map[string]any)["messages"].([]any)[0].(map[string]any)
	if page["created_at"] != "2026-11-22T19:35:23.123456789+02:00" || page["acknowledged_at"] != "2026-11-22T19:40:00+02:00" {
		t.Fatalf("list_messages times = %v, %v", page["created_at"], page["acknowledged_at"])
	}
}

func TestLocalTimeKeepsTheInstantAndLeavesOtherTextAlone(t *testing.T) {
	for _, value := range []string{"", "not a time", "2026-11-22 17:35:23"} {
		if got := localTime(value); got != value {
			t.Fatalf("localTime(%q) = %q, want it unchanged", value, got)
		}
	}
	if got := localTime("2026-11-22T17:35:23Z"); got != "2026-11-22T19:35:23+02:00" {
		t.Fatalf("localTime = %q", got)
	}
}

func TestRateLimitRetryTimeIsLocalInMCPErrorsOnly(t *testing.T) {
	err := model.NewError(model.CodeRateLimited, "rate limited: pair short bucket to cursor:x; retry in 3s (at 2026-11-01T10:00:02.623456789Z); wait or reply less often")
	want := "rate_limited: rate limited: pair short bucket to cursor:x; retry in 3s (at 2026-11-01T12:00:02.623456789+02:00); wait or reply less often"
	if got := localErrorText(err); got != want {
		t.Fatalf("rate limit text = %q, want %q", got, want)
	}
	if !strings.Contains(err.Error(), "(at 2026-11-01T10:00:02.623456789Z)") {
		t.Fatal("the error itself changed; only the MCP text may")
	}
	other := model.NewError(model.CodeInvalidInput, "not a rate limit (at 2026-11-01T10:00:02Z)")
	if got := localErrorText(other); got != other.Error() {
		t.Fatalf("another error changed: %q", got)
	}
}

// The real quota refusal goes through the same tool output as every error. Its
// retry time is shown to the agent in local time with an offset. Writing
// err.Error() into the output again must fail this test.
func TestRealFloodRetryErrorShowsLocalTimeInToolOutput(t *testing.T) {
	now := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	err := model.FloodRetryError("pair short bucket", "cursor:x", now, now.Add(3*time.Second))
	output := toolOutput(&fakeBus{}, 0, err)
	if output.Error == nil || output.Error.Code != model.CodeRateLimited {
		t.Fatalf("output error = %+v", output.Error)
	}
	if !strings.Contains(output.Error.Message, "(at 2026-11-01T12:00:03+02:00)") || strings.Contains(output.Error.Message, "T10:00:03Z") {
		t.Fatalf("the retry time is not local with an offset: %q", output.Error.Message)
	}
	if !strings.Contains(err.Error(), "(at 2026-11-01T10:00:03Z)") {
		t.Fatalf("the real error text changed (the plugin feed uses it): %q", err.Error())
	}
}
