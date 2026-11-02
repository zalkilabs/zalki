package mcpserver

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/zalkilabs/zalki/internal/bus"
)

func TestMCPDiscoveryMarksOnlyAuthenticatedCaller(t *testing.T) {
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	store := newTestStore(t)
	actors := make([]string, 2)
	keys := make([]string, 2)
	for i, id := range []string{"00000000-0000-4000-8000-000000000041", "00000000-0000-4000-8000-000000000042"} {
		v, key, err := store.Register("cursor", id, "/fixture", nil)
		if err != nil {
			t.Fatal(err)
		}
		actors[i], keys[i] = v.ID, key
	}
	client, ctx := testClient(t, &bus.Service{Store: store})
	for _, caller := range []int{-1, 0, 1, -1} {
		args := map[string]any{}
		if caller >= 0 {
			args["actor"], args["capability"] = actors[caller], keys[caller]
		}
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: args})
		if err != nil || result.IsError {
			t.Fatalf("discovery failed: %+v %v", result, err)
		}
		var output Output[[]bus.AgentInfo]
		encoded, _ := json.Marshal(result.StructuredContent)
		if err := json.Unmarshal(encoded, &output); err != nil || len(output.Data) != 2 {
			t.Fatal("discovery lost rows", err)
		}
		for _, row := range output.Data {
			wantSelf := caller >= 0 && row.ID == actors[caller]
			if row.Self != wantSelf {
				t.Fatal("self marker leaked across callers")
			}
		}
	}
	for _, args := range []map[string]any{
		{"actor": actors[0]},
		{"capability": keys[0]},
		{"actor": "", "capability": ""},
		{"actor": actors[0], "capability": ""},
		{"actor": actors[0], "capability": "wrong"},
		{"actor": actors[0], "capability": keys[1]},
	} {
		result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: args})
		if err != nil || !result.IsError {
			t.Fatal("invalid discovery credentials fell back to an anonymous roster", err)
		}
		encoded, _ := json.Marshal(result)
		if strings.Contains(string(encoded), "/fixture") || strings.Contains(string(encoded), keys[0]) || strings.Contains(string(encoded), keys[1]) {
			t.Fatal("invalid discovery returned a roster or credentials")
		}
	}
}

func TestMCPDiscoverySkipsUnreadableSessionsWithOneOptionalCount(t *testing.T) {
	t.Setenv("CURSOR_CONFIG_DIR", t.TempDir())
	service := newTestService(t)
	store := service.Store
	healthy, _, future, _ := registerPair(t, store, "cursor", "00000000-0000-4000-8000-000000000051", "cursor", "00000000-0000-4000-8000-000000000052")
	path := filepath.Join(store.Dir, "sessions", strings.ReplaceAll(future.ID, ":", "_")+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	record["schema"] = json.RawMessage(`99`)
	record["future_field"] = json.RawMessage(`true`)
	raw, _ = json.Marshal(record)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	client, ctx := testClient(t, service)
	result, err := client.CallTool(ctx, &mcp.CallToolParams{Name: "list_agents", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatal("partial discovery failed", err)
	}
	encoded, _ := json.Marshal(result.StructuredContent)
	var output DiscoveryOutput
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.SkippedSessions != 1 || len(output.Data) != 1 || output.Data[0].ID != healthy.ID {
		t.Fatal("skip count or healthy row lost", string(encoded))
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(raw, after) {
		t.Fatal("discovery changed future record")
	}
	old, _ := json.Marshal(Output[[]bus.AgentInfo]{Data: output.Data})
	clean, _ := json.Marshal(DiscoveryOutput{Data: output.Data})
	if !bytes.Equal(old, clean) {
		t.Fatal("healthy envelope changed", string(old), string(clean))
	}
}
