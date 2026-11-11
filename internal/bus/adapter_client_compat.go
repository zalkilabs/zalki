package bus

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"io"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

//go:embed testdata/clients/*.json
var clientContracts embed.FS

var clientFixtureVersions = loadFixtureVersions()

func loadFixtureVersions() map[string][]string {
	versions := map[string][]string{}
	files, _ := clientContracts.ReadDir("testdata/clients")
	for _, file := range files {
		raw, err := clientContracts.ReadFile("testdata/clients/" + file.Name())
		if err != nil {
			panic("cannot read embedded client contract")
		}
		var contract struct {
			Kind    string `json:"kind"`
			Version string `json:"version"`
		}
		if json.Unmarshal(raw, &contract) != nil {
			panic("invalid embedded client contract")
		}
		if contract.Version == "" || contract.Version == "unknown" {
			continue
		}
		duplicate := false
		for _, version := range versions[contract.Kind] {
			if version == contract.Version {
				duplicate = true
			}
		}
		if !duplicate {
			versions[contract.Kind] = append(versions[contract.Kind], contract.Version)
		}
	}
	for _, values := range versions {
		sort.Strings(values)
	}
	return versions
}

var clientVersionPattern = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?`)

type versionBuffer struct{ buffer bytes.Buffer }

func (b *versionBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 1024 - b.buffer.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buffer.Write(p)
	}
	return n, nil
}

func execClientPath(name string) (string, error) { return exec.LookPath(name) }

// clientVersionProbeTimeout bounds one `--version` run. Tests in this package
// may raise it; production code never changes it.
var clientVersionProbeTimeout = 5 * time.Second

func probeClientVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, clientVersionProbeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--version")
	var output versionBuffer
	command.Stdout, command.Stderr = &output, io.Discard
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		return ""
	}
	return clientVersionPattern.FindString(output.buffer.String())
}

func annotateClientCompatibility(ctx context.Context, diagnostic *AgentDiagnostic) {
	diagnostic.FixtureVersions = append([]string(nil), clientFixtureVersions[diagnostic.Kind]...)
	if !diagnostic.Found {
		diagnostic.Compatibility = "not_installed"
		diagnostic.RecoveryHint = "Install this client before configuring its integration."
		return
	}
	diagnostic.Version = probeClientVersion(ctx, diagnostic.Binary)
	if diagnostic.Version == "" {
		diagnostic.Compatibility = "version_unavailable"
		diagnostic.RecoveryHint = "Check the client's --version command, then run the opt-in live smoke test."
		return
	}
	diagnostic.Compatibility = "unverified_version"
	diagnostic.RecoveryHint = "This version is not covered by recorded probes; run the opt-in live smoke test before relying on it."
	for _, version := range diagnostic.FixtureVersions {
		if version == diagnostic.Version {
			diagnostic.Compatibility = "fixture_covered"
			diagnostic.RecoveryHint = "Protocol fixtures cover this version; use a live smoke test to verify this installation."
			return
		}
	}
}

func recoveryHint(diagnostic model.SessionDiagnostic, adapter agent.Adapter) string {
	if diagnostic.Retired || diagnostic.Liveness == "exited" {
		return "Start or resume the client session; do not resend messages already submitted."
	}
	if diagnostic.HookBuildStatus == "outdated" {
		return "Restart this client session and reconnect its Zalki MCP server to load the installed build."
	}
	if hinter, ok := adapter.(agent.RecoveryHintAdapter); ok {
		if hint := hinter.RecoveryHint(diagnostic); validRecoveryHint(hint) {
			return hint
		}
	}
	return ""
}

// Adapter hints are printed by doctor; keep them to one bounded line of text.
func validRecoveryHint(hint string) bool {
	return len(hint) <= 512 && utf8.ValidString(hint) && !strings.ContainsFunc(hint, unicode.IsControl)
}

// Adapter evidence is separate from generic hook/metadata fixture coverage.
// Missing installations or unavailable versions retain their common diagnosis.
func annotateAdapterCompatibility(diagnostic *AgentDiagnostic, adapter agent.Adapter) {
	if !diagnostic.Found || diagnostic.Version == "" {
		return
	}
	policy, ok := adapter.(agent.CompatibilityAdapter)
	if !ok {
		return
	}
	status, hint := policy.Compatibility(diagnostic.Version)
	if !compatibilityStatusPattern.MatchString(status) || !validRecoveryHint(hint) {
		return
	}
	diagnostic.Compatibility, diagnostic.RecoveryHint = status, hint
}

var compatibilityStatusPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
