package bus

import (
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

type pluginDeliveryCheck func(JournalView, string, model.Message) error

// Caller already holds the Store lock. Borrow/expire a view without acquiring
// the flock again, just like withJournalView. Native mail never enters the port.
func checkPluginMessageDelivery(s Store, message model.Message) error {
	if model.ValidateSessionAddress(message.From) != nil {
		return model.NewError(model.CodeUnauthorized, "invalid delivery origin")
	}
	if !strings.HasPrefix(message.From, "plugin:") && message.FromLabel != model.PluginLabel && message.PluginBinding == "" {
		return nil
	}
	if !model.IsPluginInboxAddress(message.From) {
		return model.NewError(model.CodeUnauthorized, "invalid plugin delivery origin")
	}
	if message.PluginBinding == "" || s.PluginDelivery == nil {
		return model.NewError(model.CodeUnauthorized, "plugin delivery is not permitted now")
	}
	scope := &journalScope{store: &s, writable: true}
	defer scope.expire()
	return s.PluginDelivery(JournalView{scope: scope}, s.Dir, message)
}

// A native read cannot serve as a fallback around a refused plugin handoff.
// Native-origin history bypasses the plugin port.
func blockedPluginInbox(s Store, actor string, message model.Message) bool {
	return message.To == actor && checkPluginMessageDelivery(s, message) != nil
}
