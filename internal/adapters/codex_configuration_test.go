package adapters_test

import (
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters"
	"github.com/zalkilabs/zalki/internal/agent"
)

func TestBuiltinCodexPolicyIsValueOwned(t *testing.T) {
	get := func(registry *agent.Registry) agent.Adapter {
		t.Helper()
		adapter, err := registry.Lookup("codex")
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	}
	baseline := get(adapters.Builtin())
	disabled := get(adapters.BuiltinWithCodex(false, ""))
	if !reflect.DeepEqual(baseline, disabled) {
		t.Fatal("Builtin no longer uses default-off Codex policy")
	}
	first := get(adapters.BuiltinWithCodex(true, "/fixture/first.json"))
	samePathDisabled := get(adapters.BuiltinWithCodex(false, "/fixture/first.json"))
	second := get(adapters.BuiltinWithCodex(true, "/fixture/second.json"))
	if reflect.DeepEqual(first, samePathDisabled) || reflect.DeepEqual(first, second) {
		t.Fatal("Codex construction ignored queue policy or selected config path")
	}
	if !reflect.DeepEqual(first, get(adapters.BuiltinWithCodex(true, "/fixture/first.json"))) {
		t.Fatal("constructing another registry changed a previous policy value")
	}
}
