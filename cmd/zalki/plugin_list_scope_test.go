package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// Unknown scopes remain loadable with their real revision, but their effective
// standard scope is none. A malformed or stale revision must not mask this case.
func TestPluginListUnknownScopeIsSafe(t *testing.T) {
	for index, scope := range []string{"future-scope", "all\x1b[2J\nFORGED_OWNER_LINE", "all\u202ePRIVATE_DISPLAY", "all\t"} {
		t.Run("case-"+strconv.Itoa(index), func(t *testing.T) { checkPluginUnknownScope(t, scope) })
	}
}

// Mirror only the documented revision fixture format; the backend loader above
// proves the fixture has a valid current revision. Production uses no new parser.
func rewritePluginScopeFixture(t *testing.T, store bus.Store, scope string) {
	t.Helper()
	path := filepath.Join(store.Dir, "plugins", "demo.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	status, err := pluginfeed.NewPluginFeed(store, "demo").PluginStatus()
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{"zalki-plugin-revision/1", "installation " + status.ID, "pin " + status.Launch.PinSHA256, "scope " + strconv.Quote(scope), "status " + strconv.FormatBool(status.Access.Status), "receive " + strconv.FormatBool(status.Access.Receive), "send " + strings.Join(status.Access.Send, ","), "read " + strings.Join(status.Access.Read, ","), "settings {}"}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	fields["scope"], err = json.Marshal(scope)
	if err != nil {
		t.Fatal(err)
	}
	fields["revision"], err = json.Marshal(hex.EncodeToString(sum[:]))
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func checkPluginUnknownScope(t *testing.T, scope string) {
	t.Helper()
	store, _ := pluginOwnerFixture(t)
	installReviewInstance(t, store.Dir, "demo", "0.1.0")
	rewritePluginScopeFixture(t, store, scope)
	status, err := pluginfeed.NewPluginFeed(store, "demo").PluginStatus()
	if err != nil || status.Scope != scope || !status.Enabled {
		t.Fatal("valid unknown-scope fixture did not load", err)
	}
	for _, args := range [][]string{{"list"}, {"show", "demo"}} {
		raw, err := pluginOwnerLocal(t, store.Dir, args...)
		if err != nil {
			t.Fatal("unknown-scope display refused a valid record", err)
		}
		if bytes.Contains(raw, []byte(scope)) || bytes.Contains(raw, []byte("FORGED_OWNER_LINE")) || bytes.Contains(raw, []byte("PRIVATE_DISPLAY")) {
			t.Fatal("UNKNOWN_SCOPE_RAW_OUTPUT", strconv.QuoteToASCII(string(raw)))
		}
		if !strings.Contains(strings.ToLower(string(raw)), "scope none;") && !strings.Contains(string(raw), "Scope: none;") {
			t.Fatal("unknown scope did not display effective none", string(raw))
		}
		if !bytes.Contains(raw, []byte("0 eligible in the session snapshot")) {
			t.Fatal("unknown scope displayed active standard membership", string(raw))
		}
	}
}
