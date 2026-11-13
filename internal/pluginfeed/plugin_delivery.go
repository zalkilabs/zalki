package pluginfeed

import (
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// CheckPluginDelivery revalidates an ALREADY admitted operation at its final
// handoff. It never reserves, spends quota, reloads reply counts or applies TTL.
func CheckPluginDelivery(view bus.JournalView, root string, message model.Message) error {
	return view.WithWriteScope(root, func() error {
		_, p, err := pluginInboxTarget(root, message.From)
		if err != nil || message.PluginBinding == "" || p.Revision != message.PluginBinding || p.Launch == nil {
			return model.NewError(model.CodeUnauthorized, "plugin delivery is not permitted now")
		}
		generation, err := view.SessionGeneration(message.To)
		if err != nil || generation != message.TargetGeneration {
			return model.NewError(model.CodeUnauthorized, "plugin delivery recipient registration changed")
		}
		if message.ReplyTo != "" {
			if !mayAnswer(view, p, message.To) {
				return errOutOfScope
			}
			_, err := replyParentTime(view, p, message.To, message.ReplyTo)
			return err
		}
		if !extendedMember(view, p, p.Access.Send, message.To) {
			return model.NewError(model.CodeUnauthorized, "plugin delivery is not permitted now")
		}
		return nil
	})
}
