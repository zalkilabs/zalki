package main

import (
	"bytes"
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/adapters/claude"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/config"
)

func TestDeliveryRegistryCarriesTheBuildVersionToCodex(t *testing.T) {
	registry := newCommandOptions(t.TempDir()).registryFactory(false, "")
	adapter, err := registry.Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	version := reflect.ValueOf(adapter).FieldByName("clientVersion").String()
	if version == "" || version != bus.Version {
		t.Fatalf("Codex client version = %q, want the build version %q", version, bus.Version)
	}
}

func TestCursorAdapterOffersTheListenerCapability(t *testing.T) {
	adapter, err := adapters.Builtin().Lookup("cursor")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := adapter.(agent.Listener); !ok {
		t.Fatal("the Cursor adapter is not an agent.Listener")
	}
}

func TestCursorListenNeedsTheCursorAdapter(t *testing.T) {
	registry, err := agent.NewRegistry(claude.New())
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	opts := &commandOptions{home: home, registry: registry, snapshot: &config.Snapshot{Paths: config.Paths{
		ConfigFile: config.Value[string]{Value: filepath.Join(home, "zalki.json")}, StateDir: config.Value[string]{Value: filepath.Join(home, "state")},
	}}}
	t.Setenv("ZALKI_CAPABILITY", "capability")
	cmd := newCursorListenCommand(opts)
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--actor", "cursor:00000000-0000-4000-8000-000000000000"})
	err = cmd.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(err.Error(), `unsupported agent "cursor"`) {
		t.Fatalf("error = %v, want unsupported agent cursor", err)
	}
}
