package bus

import (
	"strings"

	"github.com/zalkilabs/zalki/internal/model"
)

// withLabel fills the sender display label for records written before labels
// were snapshotted. The result is for rendering only and is never saved.
func (s Store) withLabel(msg Message) Message {
	if msg.Kind == model.NoticeKind || msg.FromLabel != "" {
		return msg
	}
	kind, _, _ := strings.Cut(msg.From, ":")
	if adapter, err := s.registry().Lookup(kind); err == nil {
		msg.FromLabel = adapter.Descriptor().Label
	}
	return msg
}
