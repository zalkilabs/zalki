package claude

import (
	_ "embed"
	"encoding/json"
	"runtime"
	"slices"
)

//go:embed testdata/socket-versions.json
var socketVersionData []byte

type versionEvidence struct {
	Status    string   `json:"status"`
	Platforms []string `json:"platforms"`
	Hint      string   `json:"hint"`
}

var socketVersions = func() map[string]versionEvidence {
	var versions map[string]versionEvidence
	if err := json.Unmarshal(socketVersionData, &versions); err != nil || len(versions) == 0 {
		panic("invalid Claude socket version evidence")
	}
	return versions
}()

// Compatibility uses exact versions; nearby releases inherit no verification.
// A verified version still provides no per-message native acknowledgement.
func (Adapter) Compatibility(version string) (string, string) {
	return compatibilityFor(version, runtime.GOOS+"/"+runtime.GOARCH)
}

func compatibilityFor(version, platform string) (string, string) {
	if evidence, ok := socketVersions[version]; ok {
		if len(evidence.Platforms) == 0 || slices.Contains(evidence.Platforms, platform) {
			return evidence.Status, evidence.Hint
		}
	}
	return "unverified_version", "Claude socket version is unverified on this platform; transport_written proves only a local write. Verify in disposable sessions with make smoke-live; see docs/design/claude-delivery-ack.md."
}
