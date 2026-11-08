package bus_test

import (
	"reflect"
	"testing"

	agentapi "github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestRegistryConfigurationValidationOrder(t *testing.T) {
	good := declared{Path: ".a/one.json", Draft: "one.json", JSON: jsonPatch("hooks")}
	const unsafe = "alpha declares an unsafe configuration path"
	const unique = "agent configuration paths and draft names must be unique"
	const format = "configuration must declare exactly one JSON patch or TOML fragment"
	for _, test := range []struct {
		name  string
		files []declared
		err   string
	}{
		{"unsafe before reserved draft and payload", []declared{{Path: "../outside", Draft: "targets.json", JSON: map[string]any{}, TOML: "x = 1"}}, unsafe},
		{"nested draft before missing payload", []declared{{Path: ".a/one.json", Draft: "sub/one.json"}}, unsafe},
		{"reserved draft before missing payload", []declared{{Path: ".a/one.json", Draft: "targets.json"}}, unique},
		{"duplicate path before missing payload", []declared{good, {Path: good.Path, Draft: "two.json"}}, unique},
		{"duplicate draft before empty JSON", []declared{good, {Path: ".a/two.json", Draft: good.Draft, JSON: map[string]any{}}}, unique},
		{"both formats before empty JSON", []declared{{Path: ".a/one.json", Draft: "one.json", JSON: map[string]any{}, TOML: "x = 1"}}, format},
		{"earlier empty JSON before later unsafe path", []declared{{Path: ".a/one.json", Draft: "one.json", JSON: map[string]any{}}, {Path: "../outside", Draft: "two.json"}}, "configuration JSON patch must not be empty"},
		{"earlier ambiguous format before later unsafe path", []declared{{Path: ".a/one.json", Draft: "one.json", JSON: jsonPatch("hooks"), TOML: "x = 1"}, {Path: "../outside", Draft: "two.json"}}, format},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkConfigurationFailureOrder(t, test.files, test.err)
		})
	}
}

func checkConfigurationFailureOrder(t *testing.T, declarations []declared, want string) {
	t.Helper()
	var calls []string
	alpha := configAdapter{nativeAdapter("alpha"), func(string, []string) []declared {
		calls = append(calls, "alpha")
		return declarations
	}}
	beta := configAdapter{nativeAdapter("beta"), func(string, []string) []declared {
		calls = append(calls, "beta")
		return []declared{{Path: ".b/ok.json", Draft: "beta.json", JSON: jsonPatch("hooks")}}
	}}
	// Reverse registration order: validation must visit alpha first and stop.
	files, err := registryOf(t, beta, alpha).Configuration("/bin/zalki", "/state")
	if err == nil || err.Error() != want || files != nil {
		t.Fatalf("files=%v error=%v, want nil files and %q", files, err, want)
	}
	if !reflect.DeepEqual(calls, []string{"alpha"}) {
		t.Fatalf("callbacks=%v, want only alpha", calls)
	}
	if got := model.ErrorCodeOf(err); got != model.CodeInternal {
		t.Fatalf("error code=%s, want internal", got)
	}
}

func TestRegistryConfigurationInputOrder(t *testing.T) {
	for _, test := range []struct {
		name, binary, state, err string
		nilRegistry              bool
	}{
		{"nil registry before relative inputs", "zalki", "state", "internal: agent registry is not configured", true},
		{"relative binary before callbacks", "zalki", "/state", "binary and state paths must be absolute", false},
		{"relative state before callbacks", "/bin/zalki", "state", "binary and state paths must be absolute", false},
		{"empty inputs before callbacks", "", "", "binary and state paths must be absolute", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls int
			var registry *agentapi.Registry
			if !test.nilRegistry {
				registry = registryOf(t, configAdapter{nativeAdapter("alpha"), func(string, []string) []declared {
					calls++
					return nil
				}})
			}
			files, err := registry.Configuration(test.binary, test.state)
			if err == nil || err.Error() != test.err || files != nil || calls != 0 {
				t.Fatalf("files=%v error=%v callbacks=%d; want nil, %q, zero callbacks", files, err, calls, test.err)
			}
		})
	}
}
