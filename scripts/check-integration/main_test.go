package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequiredIntegrationGate(t *testing.T) {
	const pkg = "github.com/zalkilabs/zalki/internal/adapters"
	event := func(action, name, path string) string {
		data, err := json.Marshal(map[string]string{"Action": action, "Test": name, "Package": path})
		if err != nil {
			t.Fatal(err)
		}
		return string(data) + "\n"
	}
	run := event("run", "TestSocket", pkg)
	pass := event("pass", "TestSocket", pkg)
	for _, test := range []struct {
		name, log, want string
	}{
		{"executed", run + pass, ""},
		{"missing", "", "missing run/pass"},
		{"cached or forged pass", pass, "missing run/pass"},
		{"unfinished", run, "missing run/pass"},
		{"wrong package", event("run", "TestSocket", "other") + event("pass", "TestSocket", "other"), "missing run/pass"},
		{"socket denied", run + event("skip", "TestSocket", pkg), "skipped"},
		{"hidden subtest skip", run + event("skip", "TestSocket/echo", pkg) + pass, "skipped"},
		{"failed", run + event("fail", "TestSocket", pkg), "failed"},
		{"allowed helpers and live opt-in", run + event("skip", "TestLiveClients", pkg) + event("skip", "TestSmokeClientHelper", pkg) + pass, ""},
		{"malformed", run + "broken", "invalid go test JSON"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := check(strings.NewReader(test.log), []requiredTest{{"adapters", "TestSocket"}})
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("wanted %q, got %v", test.want, err)
			}
		})
	}
}
