package bus

import (
	"github.com/zalkilabs/zalki/internal/agent"
)

func (s Store) registry() *agent.Registry { return s.Registry }
