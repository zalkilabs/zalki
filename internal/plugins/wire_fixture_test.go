package plugins

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
	"github.com/zalkilabs/zalki/internal/pluginfeed"
)

// wireRow is one line of a published plugin-wire fixture.
type wireRow struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	Base  string `json:"base"`
	Bytes string `json:"bytes_utf8"`
}

func readWireRows(t *testing.T, path string) []wireRow {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var rows []wireRow
	decoder := json.NewDecoder(file)
	for {
		var row wireRow
		err := decoder.Decode(&row)
		if errors.Is(err, io.EOF) {
			return rows
		}
		if err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
}

// hostFixtureFrames builds every host frame of frames.jsonl from the host's
// own types, so the published fixture cannot drift from what the host writes.
func hostFixtureFrames(t *testing.T, plugin map[string]frame) map[string]any {
	t.Helper()
	const u = "00000000-0000-4000-8000-0000000000"
	inbox := pluginfeed.PluginInboxMessage{ID: u + "10", From: "claude:" + u + "11", SenderGeneration: u + "12", To: "plugin:" + u + "13", CreatedAt: "2026-11-08T00:00:01Z", Body: "may I build now?"}
	enabled, missing, err := negotiate(plugin["plugin_hello"], hostCapabilities)
	if err != nil {
		t.Fatal("the published plugin hello does not negotiate", err)
	}
	reply := func(op, id, code string, result map[string]any) v2Reply {
		return v2Reply{Reply: &v2Result{Op: op, RequestID: id, Code: code, Result: result}}
	}
	event, gap := plugin["host:event"], plugin["host:enabled_gap"]
	return map[string]any{
		"host_hello":                  map[string]hostHello{"hello": {Wire: wireName, Host: hostInfo{Version: "0.8.0", Capabilities: hostCapabilities}}},
		"hello_ok":                    map[string]helloOK{"hello_ok": {Enabled: enabled, MissingOptional: missing}},
		"event":                       frame{ID: event.ID, Event: event.Event},
		"enabled_gap":                 frame{ID: gap.ID, Gap: "enabled"},
		"inbox_next_empty":            reply("inbox_next", "n1", "ok", nil),
		"inbox_next_rate_limited":     reply("inbox_next", "n2", "rate_limited", map[string]any{"retry_at": "2026-11-08T00:00:02Z"}),
		"inbox_next_message":          reply("inbox_next", "n3", "ok", map[string]any{"message": inbox}),
		"inbox_consume_ok":            reply("inbox_consume", "c1", "ok", map[string]any{"message": inbox.ID}),
		"send_ok":                     reply("send", "s1", "ok", map[string]any{"message": u + "14", "status": string(model.StatusPendingActivity)}),
		"send_reply_limit":            reply("send", "s2", "reply_limit", nil),
		"read_denied":                 reply("read", "r1", "denied", nil),
		"terminal_revoked":            map[string]string{"code": "revoked"},
		"terminal_stale_binding":      map[string]string{"code": "stale_binding"},
		"terminal_protocol_error":     map[string]string{"code": "protocol_error"},
		"terminal_unsupported_wire":   map[string]string{"code": "unsupported_wire"},
		"terminal_missing_capability": map[string]any{"code": "missing_capability", "missing": []string{"members"}},
	}
}

func sameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	var x, y any
	for _, pair := range []struct {
		from any
		to   *any
	}{{a, &x}, {b, &y}} {
		raw, ok := pair.from.([]byte)
		if !ok {
			var err error
			if raw, err = json.Marshal(pair.from); err != nil {
				t.Fatal(err)
			}
		}
		if err := json.Unmarshal(raw, pair.to); err != nil {
			t.Fatal(err)
		}
	}
	return reflect.DeepEqual(x, y)
}

// Every published frame is one the host writes, or one the host accepts.
func TestPublishedWireFramesMatchTheHost(t *testing.T) {
	rows := readWireRows(t, "testdata/plugin-wire/frames.jsonl")
	host := hostFixtureFrames(t, decodeWireRows(t, rows))
	seen := 0
	for _, row := range rows {
		if row.From != "host" {
			continue
		}
		want, ok := host[row.ID]
		if !ok {
			t.Fatalf("%s: no host builder for this fixture", row.ID)
		}
		if !sameJSON(t, []byte(row.Bytes), want) {
			raw, _ := json.Marshal(want)
			t.Fatalf("%s: fixture %s, host writes %s", row.ID, row.Bytes, raw)
		}
		seen++
	}
	if seen != len(host) {
		t.Fatalf("%d host fixtures, %d builders", seen, len(host))
	}
}

// decodeWireRows decodes every plugin row as the host does (each must be a
// valid hello, ack or request), and the event and gap offers as "host:ID".
func decodeWireRows(t *testing.T, rows []wireRow) map[string]frame {
	t.Helper()
	decoded := map[string]frame{}
	for _, row := range rows {
		if !strings.HasSuffix(row.Bytes, "\n") || strings.Count(row.Bytes, "\n") != 1 {
			t.Fatalf("%s: not one frame line", row.ID)
		}
		key := row.ID
		if row.From != "plugin" {
			if row.ID != "event" && row.ID != "enabled_gap" {
				continue
			}
			key = "host:" + row.ID
		}
		f, err := readFrame(bufio.NewReader(strings.NewReader(row.Bytes)))
		if err != nil {
			t.Fatalf("%s: the host refuses this fixture: %v", row.ID, err)
		}
		if _, ok := parseCommand(f); row.From == "plugin" && f.Hello == nil && !ok {
			t.Fatalf("%s: not a valid request or ack", row.ID)
		}
		decoded[key] = f
	}
	return decoded
}

// The must-ignore fixture: every host frame again, with unknown fields at
// each level (and one with unknown enum values). Without the unknown fields
// each line is its base frame, so a plugin that passes it reads the same data.
func TestHostFramesWithExtraFieldsAreSupersets(t *testing.T) {
	base := map[string]string{}
	for _, row := range readWireRows(t, "testdata/plugin-wire/frames.jsonl") {
		if row.From == "host" {
			base[row.ID] = row.Bytes
		}
	}
	covered := map[string]bool{}
	for _, row := range readWireRows(t, "testdata/plugin-wire/host-frames-with-extra-fields.jsonl") {
		covered[checkSuperset(t, row, base)] = true
	}
	for id := range base {
		if !covered[id] {
			t.Fatalf("host frame %s has no extra-fields line", id)
		}
	}
}

// checkSuperset checks one extra-fields line against its base frame and
// returns the base ID.
func checkSuperset(t *testing.T, row wireRow, base map[string]string) string {
	t.Helper()
	id := row.ID
	if row.Base != "" {
		id = row.Base
	}
	want, ok := base[id]
	if !ok {
		t.Fatalf("%s: no base host frame", row.ID)
	}
	var got, plain any
	if json.Unmarshal([]byte(row.Bytes), &got) != nil || json.Unmarshal([]byte(want), &plain) != nil {
		t.Fatalf("%s: not JSON", row.ID)
	}
	if row.Base != "" {
		return id // a deliberate unknown-values line, not a superset
	}
	if reflect.DeepEqual(got, plain) {
		t.Fatalf("%s: no unknown field", row.ID)
	}
	if !reflect.DeepEqual(dropUnknown(got, plain), plain) {
		t.Fatalf("%s: differs from its base beyond unknown fields", row.ID)
	}
	return id
}

// dropUnknown removes object keys that the base value does not have.
func dropUnknown(value, base any) any {
	switch v := value.(type) {
	case map[string]any:
		b, _ := base.(map[string]any)
		out := map[string]any{}
		for key, inner := range v {
			if known, ok := b[key]; ok {
				out[key] = dropUnknown(inner, known)
			}
		}
		return out
	case []any:
		b, _ := base.([]any)
		out := make([]any, len(v))
		for i := range v {
			var known any
			if i < len(b) {
				known = b[i]
			}
			out[i] = dropUnknown(v[i], known)
		}
		return out
	}
	return value
}

// The host stays strict on plugin frames: an unknown field anywhere in a
// plugin frame is a protocol error.
func TestHostRefusesUnknownFieldsFromAPlugin(t *testing.T) {
	for _, raw := range []string{
		`{"hello":{"wire":"zalki-plugin/1","requires":["feed"],"x":1}}`,
		`{"ack":"a","x":1}`,
		`{"op":"inbox_next","request_id":"n","x":1}`,
		`{"op":"inbox_consume","request_id":"c","args":{"message":"m","x":1}}`,
		`{"op":"send","request_id":"s","idempotency_key":"k","args":{"to":"t","body":"b","x":1}}`,
	} {
		f, err := readFrame(bufio.NewReader(strings.NewReader(raw + "\n")))
		accepted := err == nil && f.Hello != nil
		if err == nil && f.Hello == nil {
			_, accepted = parseCommand(f)
		}
		if accepted {
			t.Errorf("accepted %s", raw)
		}
	}
}

// enumFields maps a fixture field to its list in plugin-enums.json.
var enumFields = map[string]string{"type": "event_type", "state": "session_state", "previous": "session_state", "cause": "event_cause", "status": "delivery_status", "kind": "message_kind"}

// wireCodes are the reply and terminal codes the fixtures may use.
var wireCodes = []string{"ok", "denied", "rate_limited", "reply_limit", "not_a_reply", "reply_expired", "out_of_scope", "too_large", "native_deferred", "unauthorized", "conflict", "invalid_input", "unavailable", "revoked", "stale_binding", "protocol_error", "unsupported_wire", "missing_capability"}

// Every enum value in the published fixtures is in plugin-enums.json, and
// every code is a known wire code. Only lines marked as unknown values (an
// id with "unknown_values") may use others, on purpose.
func TestWireFixtureValuesArePublished(t *testing.T) {
	raw, err := os.ReadFile("../model/testdata/plugin-enums.json")
	if err != nil {
		t.Fatal(err)
	}
	var lists map[string][]struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &lists); err != nil {
		t.Fatal(err)
	}
	published := map[string]map[string]bool{}
	for list, values := range lists {
		published[list] = map[string]bool{}
		for _, v := range values {
			published[list][v.Value] = true
		}
	}
	for _, path := range []string{"testdata/plugin-wire/frames.jsonl", "testdata/plugin-wire/host-frames-with-extra-fields.jsonl"} {
		for _, row := range readWireRows(t, path) {
			var value any
			if err := json.Unmarshal([]byte(row.Bytes), &value); err != nil {
				t.Fatal(row.ID, err)
			}
			unknownOK := strings.Contains(row.ID, "unknown_values")
			checkFixtureValues(t, row.ID, value, published, unknownOK)
		}
	}
}

func checkFixtureValues(t *testing.T, id string, value any, published map[string]map[string]bool, unknownOK bool) {
	t.Helper()
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			checkFixtureValues(t, id, item, published, unknownOK)
		}
	case map[string]any:
		for key, inner := range v {
			text, isText := inner.(string)
			switch list, enum := enumFields[key]; {
			case isText && enum && !published[list][text] && !unknownOK:
				t.Errorf("%s: %s %q is not in plugin-enums.json %s", id, key, text, list)
			case isText && key == "code" && !slices.Contains(wireCodes, text):
				t.Errorf("%s: unknown code %q", id, text)
			}
			checkFixtureValues(t, id, inner, published, unknownOK)
		}
	}
}
