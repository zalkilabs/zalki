package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/install"
	"github.com/zalkilabs/zalki/internal/model"
)

type declarationFixture struct{ files []model.ConfigDeclaration }

func (declarationFixture) Descriptor() agent.AdapterDescriptor {
	return agent.AdapterDescriptor{Kind: "fixture", Label: "Fixture"}
}
func (declarationFixture) ReceiveCapabilities(bus.Session) model.ReceiveCapabilities {
	return model.ReceiveCapabilities{Delivery: "fixture", Inbox: model.InboxNative, BusyDelivery: model.DeliveryNextTurn}
}
func (declarationFixture) Deliver(context.Context, bus.Message, bus.Session) bus.Receipt {
	panic("configuration conversion must not deliver messages")
}
func (a declarationFixture) Configuration(string, []string) []model.ConfigDeclaration { return a.files }

func TestInstallDeclarationsPreservesFieldsAndJSONSharing(t *testing.T) {
	patch := map[string]any{"fixture": map[string]any{"enabled": true}}
	registry, err := agent.NewRegistry(declarationFixture{files: []model.ConfigDeclaration{
		{Path: ".b/config.toml", Draft: "b.toml", TOML: "setting = true\n", ConfigRootEnv: "B_CONFIG_ROOT"},
		{Path: ".a/config.json", Draft: "a.json", JSON: patch, ConfigRootEnv: "A_CONFIG_ROOT"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := installDeclarations(registry)("/fixture/zalki", "/fixture/state")
	want := []install.ConfigFile{
		{Path: ".a/config.json", Draft: "a.json", JSON: patch, ConfigRootEnv: "A_CONFIG_ROOT"},
		{Path: ".b/config.toml", Draft: "b.toml", TOML: "setting = true\n", ConfigRootEnv: "B_CONFIG_ROOT"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("converted declarations = %+v, %v; want %+v", got, err, want)
	}
	got[0].JSON["added"] = true
	if patch["added"] != true {
		t.Fatal("conversion copied the caller-owned JSON map")
	}
}

func TestInstallDeclarationsPreservesRegistryFailure(t *testing.T) {
	var registry *agent.Registry
	_, wantErr := registry.Configuration("/fixture/zalki", "/fixture/state")
	got, err := installDeclarations(registry)("/fixture/zalki", "/fixture/state")
	if got != nil || err == nil || err.Error() != wantErr.Error() || model.ErrorCodeOf(err) != "internal" {
		t.Fatalf("registry refusal changed: files %+v error %v", got, err)
	}
}
