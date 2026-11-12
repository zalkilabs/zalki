package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRequiredIntegrationGate(t *testing.T) {
	const pkg = "github.com/zalkilabs/zalki/internal/adapters/cursor"
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
		{"old parent package", event("run", "TestSocket", "github.com/zalkilabs/zalki/internal/adapters") + event("pass", "TestSocket", "github.com/zalkilabs/zalki/internal/adapters"), "missing run/pass"},
		{"wrong package", event("run", "TestSocket", "other") + event("pass", "TestSocket", "other"), "missing run/pass"},
		{"socket denied", run + event("skip", "TestSocket", pkg), "skipped"},
		{"hidden subtest skip", run + event("skip", "TestSocket/echo", pkg) + pass, "skipped"},
		{"failed", run + event("fail", "TestSocket", pkg), "failed"},
		{"allowed helpers and live opt-in", run + event("skip", "TestLiveClients", pkg) + event("skip", "TestSmokeClientHelper", pkg) + pass, ""},
		{"malformed", run + "broken", "invalid go test JSON"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := check(strings.NewReader(test.log), []requiredTest{{"adapters/cursor", "TestSocket"}})
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

// A required test in cmd/zalki is matched under the module root, not internal/.
func TestRequiredIntegrationGateCommandPackage(t *testing.T) {
	event := func(action, path string) string {
		data, err := json.Marshal(map[string]string{"Action": action, "Test": "TestPTY", "Package": path})
		if err != nil {
			t.Fatal(err)
		}
		return string(data) + "\n"
	}
	required := []requiredTest{{"cmd/zalki", "TestPTY"}}
	const cmd = "github.com/zalkilabs/zalki/cmd/zalki"
	if err := check(strings.NewReader(event("run", cmd)+event("pass", cmd)), required); err != nil {
		t.Fatal(err)
	}
	for log, want := range map[string]string{
		event("run", cmd) + event("skip", cmd): "skipped",
		event("run", "github.com/zalkilabs/zalki/internal/cmd/zalki") + event("pass", "github.com/zalkilabs/zalki/internal/cmd/zalki"): "missing run/pass",
	} {
		if err := check(strings.NewReader(log), required); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("wanted %q, got %v", want, err)
		}
	}
}

// The real-binary cancel tests (Ctrl-C on a terminal, SIGTERM and SIGHUP on
// pipes) are each required exactly once.
func TestRequiredIntegrationListsInstallCancelTests(t *testing.T) {
	for _, name := range []string{
		"TestPluginInstallRealTerminalCtrlC",
		"TestPluginInstallRealTerminalCtrlCDuringBuild",
		"TestPluginInstallRealTerminalSecondCtrlCKills",
		"TestPluginInstallSignalCancels",
		"TestPluginInstallSecondSignalKills",
	} {
		count := 0
		for _, test := range required {
			if test == (requiredTest{"cmd/zalki", name}) {
				count++
			}
		}
		if count != 1 {
			t.Errorf("%s is required %d times, want 1", name, count)
		}
	}
}

// Every required root must run and pass, and a skipped descendant must never
// disappear behind a passing parent. Exercise the full list, including roots
// in both cmd/ and internal/, without weakening the generic checker.
func TestRequiredIntegrationRejectsSkipsForEveryRoot(t *testing.T) {
	var report strings.Builder
	encoder := json.NewEncoder(&report)
	for _, target := range required {
		for _, action := range []string{"run", "pass"} {
			if err := encoder.Encode(map[string]string{"Action": action, "Test": target.name, "Package": target.importPath()}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := check(strings.NewReader(report.String()), required); err != nil {
		t.Fatal(err)
	}
	for _, target := range required {
		for _, name := range []string{target.name, target.name + "/descendant"} {
			data, err := json.Marshal(map[string]string{"Action": "skip", "Test": name, "Package": target.importPath()})
			if err != nil {
				t.Fatal(err)
			}
			log := report.String() + string(data) + "\n"
			if err := check(strings.NewReader(log), required); err == nil || !strings.Contains(err.Error(), "skipped") {
				t.Fatalf("skip accepted for %s/%s: %v", target.pkg, name, err)
			}
		}
	}
}
