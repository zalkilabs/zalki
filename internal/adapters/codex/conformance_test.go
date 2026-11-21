package codex

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/agent/agenttest"
)

func TestAdapterConformance(t *testing.T) { agenttest.Run(t, New(false, "fixture.json")) }
