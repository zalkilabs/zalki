package bus

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionPluginStrictAuthorityRecord(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "cursor")
	raw := string(v.PluginPolicy)
	_, policy := pluginPolicyForTest(t, s, v.ID)
	cases := []string{"", "null", `{}`, strings.Replace(raw, `"mode":"owner"`, `"mode":"off","mode":"owner"`, 1), strings.Replace(raw, `"mode"`, `"Mode"`, 1), strings.Replace(raw, `"mode":"owner"`, `"mode":"owner","schema":1`, 1), strings.Replace(raw, `"mode":"owner"`, `"mode":"allow","allow":["b","a"]`, 1), strings.Replace(raw, `"mode":"owner"`, `"mode":"owner","invalid_env":true`, 1), strings.Replace(raw, `"mode":"owner"`, `"mode":"owner","muted":null`, 1), strings.Replace(raw, `"last_change":{`, `"last_change":{"plugin":"external","muted":true,`, 1), strings.Replace(raw, `"id":"`+policy.LastChange.ID+`"`, `"id":null`, 1),
		strings.Replace(raw, `"id":"`+policy.LastChange.ID+`"`, `"id":"AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"`, 1),
		strings.Replace(raw, `"actor":"`+v.ID+`"`, `"actor":"cursor:00000000-0000-4000-8000-000000000001"`, 1),
		strings.Replace(raw, `"at":"`+policy.LastChange.At+`"`, `"at":"not-a-time"`, 1),
		strings.Replace(raw, `"last_change":{`, `"last_change":{"muted":true,`, 1),
		strings.Replace(raw, `"actor":"`+v.ID+`"`, `"actor":"`+v.ID+`","actor":"`+v.ID+`"`, 1),
		strings.Repeat(" ", sessionPluginBytes) + raw}
	for i, invalid := range cases {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			v.PluginPolicy = json.RawMessage(invalid)
			if _, valid := decodeSessionPluginPolicy(v); valid || sessionAllowsPlugin(v, "arbiter") {
				t.Fatal("Z6_MALFORMED_POLICY_IS_CLOSED")
			}
		})
	}
}

func TestSessionPluginCapacity(t *testing.T) {
	s := fixtureStore(t)
	v, key := register(t, s, "cursor")
	for i := range sessionPluginNames {
		if err := SetSessionPlugin(s, v.ID, key, fmt.Sprintf("p%d", i), true); err != nil {
			t.Fatal(err)
		}
	}
	path, _ := s.sessionPath(v.ID)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if SetSessionPlugin(s, v.ID, key, "overflow", true) == nil {
		t.Fatal("Z6_MUTE_CAPACITY_CLOSED")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Z6_CAPACITY_REFUSAL_PRESERVES_RECORD")
	}
	if err := SetSessionPlugin(s, v.ID, key, "p0", false); err != nil {
		t.Fatal("Z6_UNMUTE_AT_CAPACITY", err)
	}
}

func TestSessionPluginLegacyCleanupDoesNotInitialize(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage(`{"mode":"owner"}`)} {
		s := fixtureStore(t)
		v, _ := register(t, s, "cursor")
		v.PluginPolicy = raw
		path, _ := s.sessionPath(v.ID)
		if err := writeJSON(path, v); err != nil {
			t.Fatal(err)
		}
		if err := s.Update(v.ID, model.StateRetired, true); err != nil {
			t.Fatal(err)
		}
		after, err := s.session(v.ID)
		var compact bytes.Buffer
		if len(after.PluginPolicy) > 0 {
			if err := json.Compact(&compact, after.PluginPolicy); err != nil {
				t.Fatal(err)
			}
		}
		if err != nil || !bytes.Equal(compact.Bytes(), raw) || !after.Retired {
			t.Fatal("Z6_CLEANUP_NEVER_REPAIRS_POLICY", err)
		}
	}
}

func TestSessionPluginTwoChangesAtIdenticalTimeHaveDistinctIDs(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "cursor")
	_, p := pluginPolicyForTest(t, s, v.ID)
	first := p.LastChange.ID
	const at = "2026-11-13T01:00:00Z"
	changeSessionPlugin(&p, v.ID, "external", true, at)
	second := p.LastChange.ID
	changeSessionPlugin(&p, v.ID, "external", false, at)
	if p.LastChange.ID == second || first == second || p.LastChange.At != at || len(p.Muted) != 0 {
		t.Fatal("Z6_TWO_REAL_MUTATIONS_DISTINCT_AUDIT_SAME_TIME")
	}
}

func TestSessionPluginOwnerNilRoundTripRotatesAudit(t *testing.T) {
	s := fixtureStore(t)
	v, _ := register(t, s, "cursor")
	_, policy := pluginPolicyForTest(t, s, v.ID)
	id := policy.LastChange.ID
	for _, owner := range []*model.ProcessRef{{PID: 800001, Start: "A", Role: "client"}, nil, {PID: 800001, Start: "A", Role: "client"}} {
		current, _ := s.session(v.ID)
		current.Owner = owner
		if err := s.locked(func() error { return saveSessionFor(s, current, "") }); err != nil {
			t.Fatal(err)
		}
		_, policy = pluginPolicyForTest(t, s, v.ID)
		if policy.LastChange.ID == id {
			t.Fatal("Z6_OWNER_NIL_ROUND_TRIP_ROTATES_AUDIT")
		}
		id = policy.LastChange.ID
	}
}

func TestSessionPluginParserRejectsNUL(t *testing.T) {
	p := parseSessionPluginEnvironment("arbiter\x00")
	if p.Mode != "off" || !p.InvalidEnv {
		t.Fatal("Z6_PARSER_NUL_INVALID_OFF")
	}
}
