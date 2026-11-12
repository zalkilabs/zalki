package main

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/config"
)

func TestConfigCodexShowSourcesAndExposure(t *testing.T) {
	for _, test := range []struct {
		name, raw, source string
		want              bool
	}{
		{"default", "", "default", false},
		{"file_false", `{"codex":{"allow_queue_fallback":false}}`, "file", false},
		{"file_true", `{"codex":{"allow_queue_fallback":true}}`, "file", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := configTestHome(t)
			path := filepath.Join(home, ".config", "zalki", "config.json")
			if test.raw != "" {
				writeConfigFixture(t, path, test.raw)
			}
			t.Setenv("ZALKI_CODEX_ALLOW_QUEUE_FALLBACK", "invalid")
			out, diagnostic, err := execute(t, home, "", "config", "show")
			var show configShowOutput
			if err != nil || diagnostic != "" || json.Unmarshal([]byte(out), &show) != nil {
				t.Fatalf("Codex inspection: err=%v diagnostic=%q", err, diagnostic)
			}
			value := show.Settings.Codex.AllowQueueFallback
			if value.Value != test.want || value.Source != test.source {
				t.Fatalf("Codex inspection value/source: %+v", value)
			}
			origin := "default codex.allow_queue_fallback"
			if test.source == "file" {
				origin = path + "#codex.allow_queue_fallback"
			}
			if value.Origin != origin || show.Settings.Codex.QueueFallbackExposure != codexQueueFallbackExposure || strings.Contains(show.Settings.Codex.QueueFallbackExposure, "\n") {
				t.Fatal("Codex inspection lost source origin or its one-line exposure notice")
			}
		})
	}
}

func TestConfigCodexHasNoFlagOverride(t *testing.T) {
	for _, flag := range []string{"--codex-allow-queue-fallback", "--allow-queue-fallback"} {
		home := configTestHome(t)
		if _, _, err := execute(t, home, "", "config", "show", flag); err == nil {
			t.Fatalf("unsupported policy flag accepted: %s", flag)
		}
	}
}

func TestConfigCodexRegistryUsesSnapshot(t *testing.T) {
	home := configTestHome(t)
	path := filepath.Join(home, "selected ' settings.json")
	writeConfigFixture(t, path, `{"codex":{"allow_queue_fallback":true}}`)
	opts := codexOptionsFixture(t, home, path)
	service, err := opts.service()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := service.Store.Registry.Lookup("codex")
	if err != nil {
		t.Fatal(err)
	}
	want, err := adapters.BuiltinWithCodex(true, path).Lookup("codex")
	if err != nil || !reflect.DeepEqual(adapter, want) {
		t.Fatal("runtime registry ignored the resolved bool or selected absolute config path", err)
	}
	writeConfigFixture(t, path, `{"codex":{"allow_queue_fallback":false}}`)
	checkCodexRegistrySnapshot(t, opts, adapter)
	fresh := codexOptionsFixture(t, home, path)
	freshService, err := fresh.service()
	if err != nil {
		t.Fatal(err)
	}
	other, err := freshService.Store.Registry.Lookup("codex")
	if err != nil || reflect.DeepEqual(adapter, other) {
		t.Fatal("fresh invocation reused another invocation's queue policy", err)
	}
}

func codexOptionsFixture(t *testing.T, home, path string) *commandOptions {
	t.Helper()
	opts := newCommandOptions(home)
	opts.root = newRootCommand(home)
	if err := opts.root.PersistentFlags().Set("config", path); err != nil {
		t.Fatal(err)
	}
	return opts
}

func checkCodexRegistrySnapshot(t *testing.T, opts *commandOptions, want any) {
	t.Helper()
	service, err := opts.service()
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := service.Store.Registry.Lookup("codex")
	if err != nil || !reflect.DeepEqual(adapter, want) {
		t.Fatal("service reloaded policy after its invocation snapshot", err)
	}
}

func TestConfigCodexPreservesInjectedRegistry(t *testing.T) {
	registry := adapters.Builtin()
	opts := &commandOptions{registry: registry, snapshot: &config.Snapshot{Paths: config.Paths{StateDir: config.Value[string]{Value: t.TempDir()}}}}
	service, err := opts.service()
	if err != nil || service.Store.Registry != registry {
		t.Fatal("explicit registry injection was replaced by builtin policy wiring", err)
	}
}
