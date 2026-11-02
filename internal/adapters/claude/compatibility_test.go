package claude

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestClaudeVersionEvidenceIsExactAndPlatformScoped(t *testing.T) {
	for _, test := range []struct{ version, platform, want, hint string }{
		{"2.1.316", "darwin/arm64", "live_verified", "not a native ACK"},
		{"2.1.316", "linux/arm64", "unverified_version", "unverified"},
		{"2.1.316", "darwin/amd64", "unverified_version", "unverified"},
		{"2.1.315", "darwin/arm64", "unverified_version", "unverified"},
		{"2.1.317", "darwin/arm64", "unverified_version", "unverified"},
		{"2.1.316-beta", "darwin/arm64", "unverified_version", "unverified"},
		{"2.1.307", "darwin/arm64", "fixture_covered", "not native socket acceptance"},
	} {
		state, hint := compatibilityFor(test.version, test.platform)
		if state != test.want || !strings.Contains(hint, test.hint) || !strings.Contains(hint, "docs/design/claude-delivery-ack.md") {
			t.Fatalf("%s %s: %s %q", test.version, test.platform, state, hint)
		}
	}
}

func TestClaudeLiveEvidencePreservesPartialReportProvenance(t *testing.T) {
	var versions map[string]struct {
		Smoke struct {
			OverallPassed bool              `json:"overall_report_passed"`
			Version       string            `json:"installed_version"`
			Cases         map[string]string `json:"claude_cases"`
			BinaryHash    *string           `json:"binary_sha256"`
			ReportHash    string            `json:"report_sha256"`
		} `json:"smoke_evidence"`
		Review struct {
			BinaryHash string `json:"binary_sha256"`
			InlineACK  bool   `json:"inline_ack"`
		} `json:"source_review"`
	}
	if err := json.Unmarshal(socketVersionData, &versions); err != nil {
		t.Fatal(err)
	}
	evidence := versions["2.1.316"]
	if evidence.Smoke.Version != "2.1.316" || evidence.Smoke.OverallPassed || evidence.Smoke.BinaryHash != nil || len(evidence.Smoke.ReportHash) != 64 || len(evidence.Review.BinaryHash) != 64 || evidence.Review.InlineACK {
		t.Fatal("version evidence overstates the historical run or native acknowledgement")
	}
	for _, name := range []string{"fresh-registration", "idle-wake", "restart-recovery", "draft-preservation"} {
		if evidence.Smoke.Cases[name] != "passed" {
			t.Fatalf("live verification lost %s evidence", name)
		}
	}
}
