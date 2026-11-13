package pluginfeed

import (
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
		if !pluginInboxAdmissionAllowed(view, p, sender) {
			return model.NewError(model.CodeUnauthorized, "plugin inbox admission denied")
		}
		reserve = func(message model.Message, now time.Time) error {
			return view.WithWriteScope(stateDir, func() error {
				index, err := f.loadPluginInboxIndex(view, p.ID, now)
				if err != nil {
					return err
				}
				return f.reservePluginInbox(index, message, generation, p.Revision, now)
			})
		}
		return nil
	})
	return reserve, err
}

// pluginInboxAdmissionAllowed: the plugin has receive access and the sender
// is a member now (scope all, live session).
func pluginInboxAdmissionAllowed(view bus.JournalView, p pluginRecord, sender string) bool {
	return p.Launch != nil && p.Access.Receive && standardMember(view, p, sender)
}
