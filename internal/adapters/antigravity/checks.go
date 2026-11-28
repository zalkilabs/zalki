package antigravity

import (
	"path/filepath"

	"github.com/zalkilabs/zalki/internal/agent"
)

var _ agent.CheckAdapter = Adapter{}

// Checks gives the doctor's checklist line about Antigravity's tool permission.
// The hooks and MCP files are checked for every client by the doctor's settings
// check, so this adds only what is Antigravity's own: a missing allow rule is
// advice, not a failure (agy asks the user, and only a headless run denies).
func (Adapter) Checks(in agent.DiagnosticInput) []agent.Check {
	if !in.Found || !filepath.IsAbs(in.Home) || !toolPermissionMissing(in.Home) {
		return nil
	}
	return []agent.Check{{
		Name:   "Antigravity: no allow rule for the Zalki tools",
		Status: "note",
		Fix:    "approve the Zalki tools when agy asks, or add mcp(" + ownedEntry + "/*) to its allow rules (a headless agy run denies them without it)",
	}}
}
