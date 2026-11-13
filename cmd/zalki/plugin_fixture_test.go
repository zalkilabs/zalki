package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

// feedFixtureScript is a feed-only plugin on the one plugin wire, written in
// plain sh so tests need no build: it says hello, then stores every offered
// event frame as events/ID.json in its data folder (written beside it, then
// renamed in, so events/ never shows a partial file) and ACKs every offer. The
// child gets HOME only (no PATH), so every tool has an absolute path.
const feedFixtureScript = `#!/bin/sh
printf '{"hello":{"wire":"zalki-plugin/1","requires":["feed"]}}\n'
/bin/mkdir -p events || exit 2
while IFS= read -r line; do
	case "$line" in
	'{"hello'*) continue ;; # the host's hello and hello_ok
	'{"code"'*) exit 0 ;; # the host's terminal frame: the run ends normally
	esac
	id=$(printf '%s\n' "$line" | /usr/bin/sed -n 's/^{"id":"\([0-9a-f-]*\)".*/\1/p')
	[ -n "$id" ] || exit 3
	case "$line" in
	*'"event"'*) printf '%s\n' "$line" > "$id.tmp" && /bin/mv "$id.tmp" "events/$id.json" ;;
	esac
	printf '{"ack":"%s"}\n' "$id"
done
`

// feedFixture writes the fixture plugin and returns its absolute path.
func feedFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feed-fixture")
	if err := os.WriteFile(path, []byte(feedFixtureScript), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixtureEvent reads one stored offer of the fixture plugin.
func fixtureEvent(path string) (model.Event, bool) {
	var offer struct {
		ID    string       `json:"id"`
		Gap   string       `json:"gap,omitempty"`
		Event *model.Event `json:"event"`
	}
	raw, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(raw, &offer) != nil || offer.Event == nil {
		return model.Event{}, false
	}
	return *offer.Event, true
}
