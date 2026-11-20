package bus

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

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
	if !diagnostic.Found {
		diagnostic.Compatibility = "not_installed"
		diagnostic.RecoveryHint = "Install this client before configuring its integration."
		return
	}
	diagnostic.Version = probeClientVersion(ctx, diagnostic.Binary)
	if diagnostic.Version == "" {
		diagnostic.Compatibility = "version_unavailable"
		diagnostic.RecoveryHint = "Check the client's --version command."
		return
	}
	diagnostic.Compatibility = "installed"
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
