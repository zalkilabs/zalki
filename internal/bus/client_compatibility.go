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
	"time"
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

type versionBuffer struct{ bytes.Buffer }

func (b *versionBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 1024 - b.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.Buffer.Write(p)
	}
	return n, nil
}

func execClientPath(name string) (string, error) { return exec.LookPath(name) }

func probeClientVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, path, "--version")
	var output versionBuffer
	command.Stdout, command.Stderr = &output, io.Discard
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		return ""
	}
	return clientVersionPattern.FindString(output.String())
}

func annotateClientCompatibility(diagnostic *AgentDiagnostic) {
	diagnostic.FixtureVersions = append([]string(nil), clientFixtureVersions[diagnostic.Kind]...)
	if !diagnostic.Found {
		diagnostic.Compatibility = "not_installed"
		diagnostic.RecoveryHint = "Install this client before configuring its integration."
		return
	}
	diagnostic.Version = probeClientVersion(diagnostic.Binary)
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

func recoveryHint(diagnostic SessionDiagnostic) string {
	if diagnostic.Retired || diagnostic.Liveness == "exited" {
		return "Start or resume the client session; do not resend messages already submitted."
	}
	if diagnostic.HookBuildStatus == "outdated" {
		return "Restart this client session and reconnect its Zalki MCP server to load the installed build."
	}
	if diagnostic.ListenerHealth == "stale" || diagnostic.ListenerHealth == "not_armed" {
		return "In this Cursor session, arm one native Shell cursor-listen task using its private startup guidance."
	}
	if diagnostic.TransportHealth == "socket_unavailable" {
		return "Restart Claude so its default integration can register a fresh messaging socket."
	}
	return ""
}
