package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

type fixtureAdapter struct {
	StandardHooks
	kind       string
	inbox      InboxMode
	deliveries *atomic.Int32
	path       string
}

type configFixtureAdapter struct {
	fixtureAdapter
	files []install.ConfigFile
}

func (a configFixtureAdapter) Configuration(string, []string) []install.ConfigFile { return a.files }

func TestAdapterRejectsInvalidDraftsAndEmptyPatches(t *testing.T) {
	for _, file := range []install.ConfigFile{
		{Path: ".fourth/mcp.json", Draft: "../escape.json", JSON: install.MCPConfig("/bin/zalki", nil, false)},
		{Path: ".fourth/mcp.json", Draft: "/", JSON: install.MCPConfig("/bin/zalki", nil, false)},
		{Path: ".fourth/mcp.json", Draft: "targets.json", JSON: install.MCPConfig("/bin/zalki", nil, false)},
		{Path: ".fourth/mcp.json", Draft: "fourth.json", JSON: map[string]any{}},
		{Path: ".fourth/mcp.json", Draft: "fourth.json", JSON: install.MCPConfig("/bin/zalki", nil, false), TOML: "also TOML"},
	} {
		adapter := configFixtureAdapter{fixtureAdapter: fixtureAdapter{kind: "fourth", inbox: InboxNative}, files: []install.ConfigFile{file}}
		registry, err := NewRegistry(adapter)
		if err != nil {
			t.Fatal(err)
		}
		home := t.TempDir()
		if _, err := install.Manage(registry.Configuration, install.Request{Action: "install", Binary: "/bin/zalki", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true}); err == nil {
			t.Fatal("invalid adapter configuration accepted")
		}
		files, err := os.ReadDir(home)
		if err != nil || len(files) != 0 {
			t.Fatal("invalid adapter configuration changed files", err)
		}
	}
}

func (a fixtureAdapter) Descriptor() AdapterDescriptor {
	return AdapterDescriptor{Kind: a.kind, Label: "Fixture", Hooks: map[string]string{"start": "SessionStart", "end": "SessionEnd", "tool": "PostToolUse"}, ProcessNames: []string{"fixture-agent"}, OwnerRole: "client"}
}

func (a fixtureAdapter) ReceiveCapabilities(Session) ReceiveCapabilities {
	return ReceiveCapabilities{Delivery: "fixture", Inbox: a.inbox, BusyDelivery: DeliveryNextTurn, LifecycleHooks: true, HookDelivery: a.inbox == InboxJournal}
}

func (a fixtureAdapter) Deliver(_ context.Context, _ Message, _ Session) Receipt {
	if a.deliveries != nil {
		a.deliveries.Add(1)
	}
	status := StatusSubmitted
	if a.inbox == InboxJournal {
		status = StatusPendingActivity
	}
	return Receipt{At: timestamp(), Status: status}
}

func (a fixtureAdapter) ConfigDir() string { return "/fixture" }
func (a fixtureAdapter) LookupName(v Session, _ map[string]map[string]string) (string, string) {
	return a.kind + " " + NativeID(v), "fixture_metadata"
}
func (a fixtureAdapter) Configuration(binary string, args []string) []install.ConfigFile {
	path := a.path
	if path == "" {
		path = "." + a.kind + "/mcp.json"
	}
	return []install.ConfigFile{{Path: path, Draft: a.kind + "-mcp.json", JSON: install.MCPConfig(binary, args, false)}}
}

func TestNewAdapterUsesCoreWithoutAgentSpecificBranches(t *testing.T) {
	for _, inbox := range []InboxMode{InboxNative, InboxJournal} {
		t.Run(string(inbox), func(t *testing.T) {
			var deliveries atomic.Int32
			adapter := fixtureAdapter{kind: "fourth-agent", inbox: inbox, deliveries: &deliveries}
			registry, err := NewRegistry(adapter)
			if err != nil {
				t.Fatal(err)
			}
			store := fixtureStore(t)
			store.Registry = registry
			from, fromKey, err := store.Register(adapter.kind, newID(), "/workspace", nil)
			if err != nil {
				t.Fatal(err)
			}
			uuid := newID()
			input, _ := json.Marshal(HookInput{SessionID: uuid, CWD: "/workspace", Event: "start"})
			service := Service{Store: store}
			var output bytes.Buffer
			if err := service.Hook(adapter.kind, bytes.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			id, err := store.sessionID(adapter.kind, uuid)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), id) {
				t.Fatal("new adapter did not receive startup context")
			}
			// Re-register to obtain the receiver's capability without parsing private context.
			to, toKey, err := store.Register(adapter.kind, uuid, "/workspace", nil)
			if err != nil {
				t.Fatal(err)
			}
			name := adapter.kind + ":" + adapter.kind + " " + uuid
			msg, err := service.Send(context.Background(), SendRequest{Actor: from.ID, Capability: fromKey, To: name, MessageID: newID(), Body: "ping"})
			if err != nil || msg.To != to.ID || msg.FromLabel != "Fixture" {
				t.Fatalf("%+v %v", msg, err)
			}
			if !strings.Contains(Envelope(msg), "From: Fixture") {
				t.Fatal("new adapter display label was lost")
			}
			incoming, err := store.Messages(to.ID, toKey)
			if err != nil || len(incoming) != 1 {
				t.Fatalf("%+v %v", incoming, err)
			}
			if incoming[0].BodyDeferred != (inbox == InboxNative) {
				t.Fatal("inbox behavior was inferred from the agent kind")
			}
			if inbox == InboxJournal {
				text, err := store.EmitPending(to.ID)
				if err != nil || !strings.Contains(text, "ping") {
					t.Fatalf("%q %v", text, err)
				}
			} else if _, err := store.EmitPending(to.ID); err == nil {
				t.Fatal("native inbox unexpectedly permitted hook delivery")
			}
			reply, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: toKey, MessageID: msg.ID, Body: "pong"})
			if err != nil || reply.To != from.ID || reply.ReplyTo != msg.ID {
				t.Fatalf("%+v %v", reply, err)
			}
			if _, err := service.Reply(context.Background(), ReplyRequest{Actor: to.ID, Capability: toKey, MessageID: msg.ID, Body: "pong"}); err != nil {
				t.Fatal(err)
			}
			if deliveries.Load() != 2 {
				t.Fatalf("expected one request and one reply; got %d deliveries", deliveries.Load())
			}
			items, err := store.List()
			if err != nil || len(items) != 2 || items[0].Receive.Inbox != inbox {
				t.Fatalf("%+v %v", items, err)
			}
			input, _ = json.Marshal(HookInput{SessionID: uuid, Event: "end"})
			if err := service.Hook(adapter.kind, bytes.NewReader(input), &output); err != nil {
				t.Fatal(err)
			}
			items, err = store.List()
			if err != nil || len(items) != 1 {
				t.Fatal("new adapter lifecycle did not retire its receiver", err)
			}
		})
	}
}

func TestRegistryRejectsInvalidAndDuplicateAdapters(t *testing.T) {
	valid := fixtureAdapter{kind: "fixture", inbox: InboxNative}
	for _, adapters := range [][]Adapter{
		{nil}, {valid, valid}, {fixtureAdapter{kind: "../escape", inbox: InboxNative}}, {fixtureAdapter{kind: "fixture", inbox: ""}},
	} {
		if _, err := NewRegistry(adapters...); err == nil {
			t.Fatal("invalid adapter registry accepted")
		}
	}
	if _, err := testRegistry().Lookup("unknown"); err == nil {
		t.Fatal("unknown adapter accepted")
	}
}

func TestBuiltinCapabilitiesDescribeInboxAndWakeRequirements(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir())
	for _, test := range []struct {
		kind                         string
		inbox                        InboxMode
		timing                       DeliveryTiming
		idle, listener, experimental bool
	}{
		{"codex", InboxNative, DeliveryNextTurn, true, false, false},
		{"claude", InboxNative, DeliveryBetweenTools, true, false, true},
		{"cursor", InboxJournal, DeliveryNextHook, false, true, false},
	} {
		t.Run(test.kind, func(t *testing.T) {
			adapter, err := testRegistry().Lookup(test.kind)
			if err != nil {
				t.Fatal(err)
			}
			c := adapter.ReceiveCapabilities(Session{})
			if c.Inbox != test.inbox || c.BusyDelivery != test.timing || c.IdleWake != test.idle || c.RequiresListener != test.listener || c.Experimental != test.experimental {
				t.Fatalf("unexpected receive profile: %+v", c)
			}
			if err := c.validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
	c := (cursorAdapter{}).ReceiveCapabilities(func() Session { v := Session{}; setCursorState(&v, cursorSessionState{Wake: &Endpoint{}}); return v }())
	if !c.IdleWake || c.Inbox != InboxJournal || c.BusyDelivery != DeliveryNotification {
		t.Fatal("armed listener did not change the advertised receive profile")
	}
}

func TestLegacyCapabilitiesAreEnrichedWithoutRewritingRecords(t *testing.T) {
	store := fixtureStore(t)
	for _, kind := range testRegistry().Kinds() {
		v, _ := register(t, store, kind)
		path, err := store.sessionPath(v.ID)
		if err != nil {
			t.Fatal(err)
		}
		var record map[string]any
		if err := readJSON(path, &record); err != nil {
			t.Fatal(err)
		}
		record["receive"] = map[string]any{"idle_wake": kind != "cursor", "delivery": "old"}
		if err := writeJSON(path, record); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		current, err := store.session(v.ID)
		if err != nil || current.Receive.Inbox == "" || current.Receive.BusyDelivery == "" || current.Receive.Delivery == "old" {
			t.Fatalf("%+v %v", current, err)
		}
		after, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(before, after) {
			t.Fatal("capability discovery rewrote an existing record", err)
		}
	}
}

func TestNewAdapterConfigurationUpgradeAndRemoval(t *testing.T) {
	clearConfigOverrides(t)
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if _, err := install.Apply(testRegistry().Configuration, "/old/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	registry, err := NewRegistry(stubCodexAdapter{}, stubClaudeAdapter{}, cursorAdapter{}, fixtureAdapter{kind: "fourth", inbox: InboxNative})
	if err != nil {
		t.Fatal(err)
	}
	before := installationSnapshot(t, home)
	if _, err := install.Manage(registry.Configuration, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: false}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("new adapter preview changed configuration")
	}
	if _, err := install.Manage(registry.Configuration, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".fourth", "mcp.json")
	var settings map[string]any
	if err := readJSON(path, &settings); err != nil {
		t.Fatal("new adapter was not installed", err)
	}
	settings["unrelated"] = "keep"
	if err := writeJSON(path, settings); err != nil {
		t.Fatal(err)
	}
	before = installationSnapshot(t, home)
	if _, err := install.Manage(registry.Configuration, install.Request{Action: "upgrade", Binary: "/new/zalki", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("new adapter upgrade was not idempotent")
	}
	if _, err := install.Manage(registry.Configuration, install.Request{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err != nil {
		t.Fatal(err)
	}
	if err := readJSON(path, &settings); err != nil || settings["unrelated"] != "keep" || len(settings["mcpServers"].(map[string]any)) != 0 {
		t.Fatal("new adapter removal changed unrelated settings", err)
	}
}

func TestAdapterConfigurationCannotClaimArbitraryPaths(t *testing.T) {
	clearConfigOverrides(t)
	for _, path := range []string{"../outside.json", "/outside.json", ".fourth/../outside.json"} {
		t.Run(path, func(t *testing.T) {
			registry, err := NewRegistry(fixtureAdapter{kind: "fourth", inbox: InboxNative, path: path})
			if err != nil {
				t.Fatal(err)
			}
			home := t.TempDir()
			if _, err := install.Manage(registry.Configuration, install.Request{Action: "install", Binary: "/bin/zalki", StateDir: filepath.Join(home, "state"), HomeDir: home, Apply: true}); err == nil {
				t.Fatal("unsafe configuration path accepted")
			}
			files, err := os.ReadDir(home)
			if err != nil || len(files) != 0 {
				t.Fatal("invalid adapter partially installed settings", err)
			}
		})
	}
	// A tampered record cannot extend ownership to an adapter-undeclared file.
	home := t.TempDir()
	state := filepath.Join(home, "state")
	if _, err := install.Apply(testRegistry().Configuration, "/bin/zalki", state, home); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(state, "installation.json")
	var record map[string]any
	if err := readJSON(path, &record); err != nil {
		t.Fatal(err)
	}
	record["patches"].(map[string]any)["unrelated.json"] = install.MCPConfig("/bin/other", nil, false)
	if err := writeJSON(path, record); err != nil {
		t.Fatal(err)
	}
	before := installationSnapshot(t, home)
	if _, err := install.Manage(testRegistry().Configuration, install.Request{Action: "uninstall", Binary: "", StateDir: state, HomeDir: home, Apply: true}); err == nil {
		t.Fatal("record claimed an undeclared configuration file")
	}
	if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
		t.Fatal("invalid ownership changed settings")
	}
}
