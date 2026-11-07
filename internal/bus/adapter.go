package bus

import (
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

const (
	InboxNative          = model.InboxNative
	InboxJournal         = model.InboxJournal
	DeliveryNextTurn     = model.DeliveryNextTurn
	DeliveryBetweenTools = model.DeliveryBetweenTools
	DeliveryNextHook     = model.DeliveryNextHook
	DeliveryNotification = model.DeliveryNotification
)

func (s Store) registry() *agent.Registry { return s.Registry }
