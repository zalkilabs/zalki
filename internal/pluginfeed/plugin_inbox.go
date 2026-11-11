package pluginfeed

import (
	"slices"
	"time"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// AdmitPluginInbox is the CLI's local plugin inbox admission implementation.
// The returned operation is used once, before the journal callback returns.
func AdmitPluginInbox(view bus.JournalView, stateDir, to, sender, generation string) (func(model.Message, time.Time) error, error) {
	var reserve func(model.Message, time.Time) error
	err := view.WithWriteScope(stateDir, func() error {
		f, p, err := pluginInboxTarget(stateDir, to)
		if err != nil {
			return err
		}
		if !pluginInboxAdmissionAllowed(p, sender) {
			return model.NewError(model.CodeUnauthorized, "plugin inbox admission denied")
		}
		reserve = func(message model.Message, now time.Time) error {
			return view.WithWriteScope(stateDir, func() error {
				index, err := f.loadPluginInboxIndex(view, p.ID, now)
				if err != nil {
					return err
				}
				return f.reservePluginInbox(index, message, generation, p.Capabilities.Revision, now)
			})
		}
		return nil
	})
	return reserve, err
}

func pluginInboxAdmissionAllowed(p pluginRecord, sender string) bool {
	c := p.Capabilities
	return p.Launch != nil && c != nil && c.Installation == p.ID && c.PinSHA256 == p.Launch.PinSHA256 && slices.Contains(c.InboxFrom, sender)
}
