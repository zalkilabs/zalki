package install_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/zalkilabs/zalki/internal/install"
)

// These tests pin validateInstallationPatch through the public path: a
// tampered patch in installation.json, refused by Manage. Single-fault texts
// for the common cases are already pinned in
// TestTamperedRecordPatchesAreRefusedWithExactMessages; this file adds the odd
// values and the cases where more than one rule is broken.
//
// The checks run in this order (installation.go, validateInstallationPatch):
//
//  1. An empty patch is refused: "empty installation patch".
//  2. Then each key of the patch is checked, in unspecified Go map order:
//     "version"    must equal 1:              "unsupported recorded hook version"
//     "mcpServers" must be an object:         "invalid recorded MCP entry"
//     then hold exactly one "zalki" object:   "invalid recorded MCP entry; expected exactly one zalki server"
//     "hooks"      must be an object:         "invalid recorded hooks"
//     then each event must be a non-empty list: "invalid recorded hook entries"
//     any other key:                          "unexpected recorded settings key"
//  3. The first fault found returns at once. Valid keys never hide a fault.
//
// Because step 2 follows map order, a patch with faults in two different keys
// is refused with either message. These tests accept that, and do not pin one
// winner. Error priority and permissive validation limits are characterized,
// not endorsed; changing either requires separate review. Before patch
// validation, the record must match the home and state (record.validate).

// tamperedPatchHome installs, then replaces the recorded patch of one settings
// file with the given JSON. It returns the home, state and the files' snapshot.
func tamperedPatchHome(t *testing.T, patch string) (home, state string, snapshot map[string]string) {
	t.Helper()
	home, state = installedHome(t)
	tamperRecord(t, state, func(record map[string]any) {
		var value any
		if err := json.Unmarshal([]byte(patch), &value); err != nil {
			t.Fatal(err)
		}
		record["patches"].(map[string]any)[".cursor/mcp.json"] = value
	})
	return home, state, installationSnapshot(t, home)
}

// tamperRecord rewrites installation.json after the given change.
func tamperRecord(t *testing.T, state string, change func(record map[string]any)) {
	t.Helper()
	path := filepath.Join(state, "installation.json")
	var record map[string]any
	if err := json.Unmarshal([]byte(readFileString(t, path)), &record); err != nil {
		t.Fatal(err)
	}
	change(record)
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPatchValidationAcceptsShapesItDoesNotLookInto(t *testing.T) {
	// A zalki server that differs from the settings file passes validation and
	// is then refused while removing, with a message that names the file.
	edited := "managed zalki MCP entry was edited; review it before removing or upgrading"
	tests := []struct{ name, patch, refusedLater string }{
		{"a bare schema version", `{"version":1}`, ""},
		{"a version written as 1.0", `{"version":1.0}`, ""},
		{"no hook events", `{"hooks":{}}`, ""},
		{"a hook event with any list item", `{"hooks":{"stop":[1]}}`, ""},
		{"a zalki server with any object", `{"mcpServers":{"zalki":{}}}`, edited},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, state, _ := tamperedPatchHome(t, test.patch)
			_, err := install.Manage(configDeclare, install.Request{Action: "uninstall", Binary: "/bin/zalki", StateDir: state, HomeDir: home, Apply: true})
			if test.refusedLater == "" && err != nil {
				t.Fatalf("a patch of this shape was refused: %v", err)
			}
			if test.refusedLater != "" && (err == nil || err.Error() != filepath.Join(home, ".cursor", "mcp.json")+": "+test.refusedLater) {
				t.Fatalf("error = %v, want a removal refusal %q", err, test.refusedLater)
			}
		})
	}
}

func TestPatchValidationRefusesOddValuesOfKnownKeys(t *testing.T) {
	const (
		version = "unsupported recorded hook version"
		mcp     = "invalid recorded MCP entry"
		hooks   = "invalid recorded hooks"
		events  = "invalid recorded hook entries"
	)
	tests := []struct{ name, patch, want string }{
		{"a null version", `{"version":null}`, version},
		{"a version written as text", `{"version":"1"}`, version},
		{"a version of true", `{"version":true}`, version},
		{"a version of 1.5", `{"version":1.5}`, version},
		{"null mcpServers", `{"mcpServers":null}`, mcp},
		{"text for mcpServers", `{"mcpServers":"zalki"}`, mcp},
		{"null hooks", `{"hooks":null}`, hooks},
		{"a list for hooks", `{"hooks":[]}`, hooks},
		{"a null hook event", `{"hooks":{"stop":null}}`, events},
		{"an object as hook event", `{"hooks":{"stop":{}}}`, events},
		{"one good and one empty hook event", `{"hooks":{"start":[1],"stop":[]}}`, events},
		{"two bad hook events", `{"hooks":{"start":[],"stop":"x"}}`, events},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, state, before := tamperedPatchHome(t, test.patch)
			_, err := install.Manage(configDeclare, install.Request{Action: "uninstall", Binary: "/bin/zalki", StateDir: state, HomeDir: home, Apply: true})
			if err == nil || err.Error() != test.want {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
				t.Fatal("a refused request changed files")
			}
		})
	}
}

func TestPatchValidationWithSeveralFaults(t *testing.T) {
	const (
		empty   = "empty installation patch"
		version = "unsupported recorded hook version"
		mcp     = "invalid recorded MCP entry"
		server  = "invalid recorded MCP entry; expected exactly one zalki server"
		hooks   = "invalid recorded hooks"
		events  = "invalid recorded hook entries"
		unknown = "unexpected recorded settings key"
	)
	const goodMCP = `"mcpServers":{"zalki":{"command":"x"}}`
	// want lists every message a patch may be refused with. A single entry is
	// a fixed winner; several entries mean the winner depends on map order.
	tests := []struct {
		name, patch string
		want        []string
	}{
		// Valid keys never hide a fault, wherever map order puts them.
		{"valid version and bad hooks", `{"version":1,"hooks":1}`, []string{hooks}},
		{"valid MCP entry and an unknown key", `{` + goodMCP + `,"weird":1}`, []string{unknown}},
		{"valid keys around a bad hook event", `{"version":1,` + goodMCP + `,"hooks":{"a":[1],"b":[]}}`, []string{events}},
		// Several faults inside one key have one message.
		{"two bad hook events with different faults", `{"hooks":{"a":[],"b":"x","c":null}}`, []string{events}},
		{"two servers and none an object", `{"mcpServers":{"zalki":"x","other":1}}`, []string{server}},
		// Faults in different keys: either message, never anything else.
		{"unknown key and bad MCP entry", `{"weird":1,"mcpServers":[]}`, []string{unknown, mcp}},
		{"unknown key and bad server set", `{"weird":1,"mcpServers":{"other":{}}}`, []string{unknown, server}},
		{"bad version and bad hooks", `{"version":2,"hooks":1}`, []string{version, hooks}},
		{"bad version and bad hook events", `{"version":2,"hooks":{"a":[]}}`, []string{version, events}},
		{"bad hooks and bad MCP entry", `{"hooks":1,"mcpServers":[]}`, []string{hooks, mcp}},
		{"every key faulty", `{"version":2,"mcpServers":[],"hooks":1,"weird":1}`, []string{version, mcp, hooks, unknown}},
	}
	// Repeat to exercise valid-before-invalid paths without requiring any
	// particular map order or requiring every allowed error to be observed.
	const attempts = 30
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home, state, before := tamperedPatchHome(t, test.patch)
			for range attempts {
				_, err := install.Manage(configDeclare, install.Request{Action: "uninstall", Binary: "/bin/zalki", StateDir: state, HomeDir: home, Apply: true})
				if err == nil || !slices.Contains(test.want, err.Error()) {
					t.Fatalf("error = %v, want one of %q", err, test.want)
				}
			}
			if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
				t.Fatal("a refused request changed files")
			}
		})
	}
	t.Run("an empty patch is the only patch refused for being empty", func(t *testing.T) {
		home, state, _ := tamperedPatchHome(t, `{}`)
		_, err := install.Manage(configDeclare, install.Request{Action: "upgrade", Binary: "/bin/zalki", StateDir: state, HomeDir: home, Apply: true})
		if err == nil || err.Error() != recordFailure(filepath.Join(state, "installation.json"), empty) {
			t.Fatalf("error = %v, want %q", err, empty)
		}
	})
}

func TestRecordMismatchIsReportedBeforeAnyPatchFault(t *testing.T) {
	const mismatch = "installation record does not match this home/state or has an unsupported schema"
	for name, change := range map[string]func(record map[string]any){
		"an unsupported schema": func(record map[string]any) { record["schema"] = 2 },
		"another home":          func(record map[string]any) { record["home"] = "/elsewhere" },
	} {
		t.Run(name, func(t *testing.T) {
			home, state, _ := tamperedPatchHome(t, `{"weird":1,"hooks":1}`)
			tamperRecord(t, state, change)
			before := installationSnapshot(t, home)
			_, err := install.Manage(configDeclare, install.Request{Action: "uninstall", Binary: "/bin/zalki", StateDir: state, HomeDir: home, Apply: true})
			if err == nil || err.Error() != mismatch {
				t.Fatalf("error = %v, want %q", err, mismatch)
			}
			if !reflect.DeepEqual(before, installationSnapshot(t, home)) {
				t.Fatal("a refused request changed files")
			}
		})
	}
}
