package bus_test

import (
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// declared is the configuration declaration type that adapters return. It is
// the only name in this file that depends on where that type lives. Tests read
// its fields by name, so changing this alias is the whole migration.
type declared = model.ConfigDeclaration

// configAdapter is a fake client that declares installation files.
type configAdapter struct {
	*fakeAdapter
	files func(binary string, args []string) []declared
}

func (a configAdapter) Configuration(binary string, args []string) []declared {
	return a.files(binary, args)
}

func jsonPatch(key string) map[string]any { return map[string]any{key: map[string]any{"on": true}} }

// declaring returns an adapter of the given kind that declares fixed files.
func declaring(kind string, files ...declared) bus.Adapter {
	return configAdapter{nativeAdapter(kind), func(string, []string) []declared { return files }}
}

func registryOf(t *testing.T, adapters ...bus.Adapter) *bus.Registry {
	t.Helper()
	registry, err := bus.NewRegistry(adapters...)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// summary reduces declarations to comparable literals, so expectations stay
// independent of the declaration type.
type summary struct {
	Path, Draft, Root, TOML string
	JSON                    map[string]any
}

func summarize(files []declared) []summary {
	out := make([]summary, 0, len(files))
	for _, file := range files {
		out = append(out, summary{Path: file.Path, Draft: file.Draft, Root: file.ConfigRootEnv, TOML: file.TOML, JSON: file.JSON})
	}
	return out
}

func TestRegistryConfigurationRefusals(t *testing.T) {
	const kind = "alpha"
	good := declared{Path: ".a/one.json", Draft: "a-one.json", JSON: jsonPatch("mcpServers")}
	with := func(change func(*declared)) declared {
		file := good
		change(&file)
		return file
	}
	tests := []struct {
		name          string
		registry      func(t *testing.T) *bus.Registry
		binary, state string
		wantText      string
		wantCode      bus.ErrorCode
	}{
		{"nil registry", func(*testing.T) *bus.Registry { return nil }, "/bin/zalki", "/state",
			"internal: agent registry is not configured", "internal"},
		{"relative binary", func(t *testing.T) *bus.Registry { return registryOf(t, declaring(kind, good)) }, "zalki", "/state",
			"binary and state paths must be absolute", "internal"},
		{"relative state", func(t *testing.T) *bus.Registry { return registryOf(t, declaring(kind, good)) }, "/bin/zalki", "state",
			"binary and state paths must be absolute", "internal"},
		{"empty binary", func(t *testing.T) *bus.Registry { return registryOf(t, declaring(kind, good)) }, "", "/state",
			"binary and state paths must be absolute", "internal"},
		{"no adapter implements the interface", func(t *testing.T) *bus.Registry { return registryOf(t, nativeAdapter(kind)) }, "/bin/zalki", "/state",
			"no adapter declares installation configuration", "internal"},
		{"an adapter that declares nothing", func(t *testing.T) *bus.Registry { return registryOf(t, declaring(kind)) }, "/bin/zalki", "/state",
			"no adapter declares installation configuration", "internal"},
		{"empty registry", func(t *testing.T) *bus.Registry { return registryOf(t) }, "/bin/zalki", "/state",
			"no adapter declares installation configuration", "internal"},
	}
	unsafe := map[string]declared{
		"parent path":            with(func(f *declared) { f.Path = "../outside.json" }),
		"absolute path":          with(func(f *declared) { f.Path = "/outside.json" }),
		"path with dot segment":  with(func(f *declared) { f.Path = ".a/../one.json" }),
		"unclean path":           with(func(f *declared) { f.Path = ".a//one.json" }),
		"dot path":               with(func(f *declared) { f.Path = "." }),
		"empty path":             with(func(f *declared) { f.Path = "" }),
		"empty draft":            with(func(f *declared) { f.Draft = "" }),
		"draft in a directory":   with(func(f *declared) { f.Draft = "dir/a-one.json" }),
		"parent draft":           with(func(f *declared) { f.Draft = "../a-one.json" }),
		"unsafe and ambiguous":   with(func(f *declared) { f.Path = "/abs.json"; f.TOML = "x = 1" }),
		"unsafe wins over empty": with(func(f *declared) { f.Path = "../x"; f.JSON = map[string]any{} }),
	}
	for name, file := range unsafe {
		tests = append(tests, struct {
			name          string
			registry      func(t *testing.T) *bus.Registry
			binary, state string
			wantText      string
			wantCode      bus.ErrorCode
		}{"unsafe: " + name, func(t *testing.T) *bus.Registry { return registryOf(t, declaring(kind, file)) }, "/bin/zalki", "/state",
			"alpha declares an unsafe configuration path", "internal"})
	}
	const unique = "agent configuration paths and draft names must be unique"
	other := declared{Path: ".b/two.json", Draft: "b-two.json", JSON: jsonPatch("hooks")}
	more := []struct {
		name  string
		files []bus.Adapter
		text  string
	}{
		{"duplicate path in one adapter", []bus.Adapter{declaring(kind, good, with(func(f *declared) { f.Draft = "other.json" }))}, unique},
		{"duplicate draft in one adapter", []bus.Adapter{declaring(kind, good, with(func(f *declared) { f.Path = ".a/two.json" }))}, unique},
		{"duplicate path across adapters", []bus.Adapter{declaring(kind, good), declaring("beta", with(func(f *declared) { f.Draft = "b.json" }))}, unique},
		{"duplicate draft across adapters", []bus.Adapter{declaring(kind, good), declaring("beta", with(func(f *declared) { f.Path = ".b/x.json" }))}, unique},
		{"the reserved draft name", []bus.Adapter{declaring(kind, with(func(f *declared) { f.Draft = "targets.json" }))}, unique},
		{"both JSON and TOML", []bus.Adapter{declaring(kind, with(func(f *declared) { f.TOML = "x = 1" }))}, "configuration must declare exactly one JSON patch or TOML fragment"},
		{"neither JSON nor TOML", []bus.Adapter{declaring(kind, with(func(f *declared) { f.JSON = nil }))}, "configuration must declare exactly one JSON patch or TOML fragment"},
		{"an empty JSON patch", []bus.Adapter{declaring(kind, with(func(f *declared) { f.JSON = map[string]any{} }))}, "configuration JSON patch must not be empty"},
		{"uniqueness is checked before the JSON/TOML rule", []bus.Adapter{declaring(kind, good, with(func(f *declared) { f.JSON = nil }))}, unique},
		{"one bad adapter refuses the whole set", []bus.Adapter{declaring(kind, other), declaring("beta", with(func(f *declared) { f.Path = "../x" }))}, "beta declares an unsafe configuration path"},
	}
	for _, test := range more {
		adapters := test.files
		tests = append(tests, struct {
			name          string
			registry      func(t *testing.T) *bus.Registry
			binary, state string
			wantText      string
			wantCode      bus.ErrorCode
		}{test.name, func(t *testing.T) *bus.Registry { return registryOf(t, adapters...) }, "/bin/zalki", "/state", test.text, "internal"})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			files, err := test.registry(t).Configuration(test.binary, test.state)
			if err == nil || err.Error() != test.wantText || files != nil {
				t.Fatalf("got %v, %v; want no files and error %q", files, err, test.wantText)
			}
			if got := bus.ErrorCodeOf(err); got != test.wantCode {
				t.Fatalf("error code %q, want %q", got, test.wantCode)
			}
		})
	}
}

func TestRegistryConfigurationResult(t *testing.T) {
	file := func(path, draft string) declared { return declared{Path: path, Draft: draft, JSON: jsonPatch(draft)} }
	t.Run("one adapter", func(t *testing.T) {
		registry := registryOf(t, declaring("alpha", declared{Path: ".a/c.toml", Draft: "c.toml", TOML: "x = 1\n", ConfigRootEnv: "ALPHA_HOME"}))
		files, err := registry.Configuration("/bin/zalki", "/state")
		want := []summary{{Path: ".a/c.toml", Draft: "c.toml", Root: "ALPHA_HOME", TOML: "x = 1\n"}}
		if err != nil || !reflect.DeepEqual(summarize(files), want) {
			t.Fatalf("got %+v, %v; want %+v", summarize(files), err, want)
		}
	})

	t.Run("three adapters come back sorted by path, not by kind or declaration order", func(t *testing.T) {
		registry := registryOf(t,
			declaring("zeta", file(".a/c.json", "c.json")),
			declaring("alpha", file(".z/a.json", "a.json"), file(".m/y.json", "y.json")),
			declaring("mid", file(".m/b.json", "b.json")),
		)
		files, err := registry.Configuration("/bin/zalki", "/state")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, f := range summarize(files) {
			got = append(got, f.Path)
		}
		want := []string{".a/c.json", ".m/b.json", ".m/y.json", ".z/a.json"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("paths %v, want %v", got, want)
		}
	})

	t.Run("adapters that do not implement the interface are skipped", func(t *testing.T) {
		registry := registryOf(t, nativeAdapter("alpha"), declaring("beta", file(".b/x.json", "x.json")))
		files, err := registry.Configuration("/bin/zalki", "/state")
		if err != nil || len(files) != 1 || files[0].Path != ".b/x.json" {
			t.Fatalf("got %+v, %v", files, err)
		}
	})

	t.Run("each adapter gets the binary and its own copy of the arguments", func(t *testing.T) {
		var seen [][]string
		var binaries []string
		record := func(kind, path, draft string, tamper bool) bus.Adapter {
			return configAdapter{nativeAdapter(kind), func(binary string, args []string) []declared {
				binaries = append(binaries, binary)
				seen = append(seen, append([]string{}, args...))
				if tamper {
					args[0] = "--tampered"
				}
				return []declared{file(path, draft)}
			}}
		}
		registry := registryOf(t, record("beta", ".b/x.json", "x.json", false), record("alpha", ".a/y.json", "y.json", true))
		for range 2 {
			if _, err := registry.Configuration("/opt/my tools/zalki", "/var/my state"); err != nil {
				t.Fatal(err)
			}
		}
		wantArgs := []string{"--state", "/var/my state"}
		// Adapters run in kind order: alpha (tampers), then beta, on every call.
		if len(seen) != 4 || !reflect.DeepEqual(seen[0], wantArgs) || !reflect.DeepEqual(seen[1], wantArgs) ||
			!reflect.DeepEqual(seen[2], wantArgs) || !reflect.DeepEqual(seen[3], wantArgs) {
			t.Fatalf("adapters saw %q", seen)
		}
		for _, binary := range binaries {
			if binary != "/opt/my tools/zalki" {
				t.Fatalf("binary %q was altered", binary)
			}
		}
	})

	t.Run("the slice is new on every call but the JSON maps are the adapter's own", func(t *testing.T) {
		patch := jsonPatch("mcpServers")
		registry := registryOf(t, declaring("alpha",
			declared{Path: ".a/two.json", Draft: "two.json", JSON: patch},
			declared{Path: ".a/one.json", Draft: "one.json", JSON: jsonPatch("hooks")}))
		first, err := registry.Configuration("/bin/zalki", "/state")
		if err != nil {
			t.Fatal(err)
		}
		first[0], first[1] = first[1], first[0] // Reordering a result must not leak into later calls.
		second, err := registry.Configuration("/bin/zalki", "/state")
		if err != nil || second[0].Path != ".a/one.json" || second[1].Path != ".a/two.json" {
			t.Fatalf("a later call saw the earlier caller's reordering: %+v, %v", summarize(second), err)
		}
		// The registry copies neither the declaration's maps nor their contents.
		second[1].JSON["added"] = true
		if patch["added"] != true {
			t.Fatal("the registry copies JSON maps; the adapter's map no longer sees a caller's change")
		}
	})

}
