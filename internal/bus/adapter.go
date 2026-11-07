package bus

import (
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

// InboxMode is the compatibility name for model.InboxMode.
type InboxMode = model.InboxMode

// DeliveryTiming is the compatibility name for model.DeliveryTiming.
type DeliveryTiming = model.DeliveryTiming

const (
	InboxNative          = model.InboxNative
	InboxJournal         = model.InboxJournal
	DeliveryNextTurn     = model.DeliveryNextTurn
	DeliveryBetweenTools = model.DeliveryBetweenTools
	DeliveryNextHook     = model.DeliveryNextHook
	DeliveryNotification = model.DeliveryNotification
)

// validKind forwards syntax validation without consulting the registry.
func validKind(kind string) bool { return model.ValidKind(kind) }

func (s Store) registry() *agent.Registry { return s.Registry }
