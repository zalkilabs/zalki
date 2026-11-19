package pluginfeed

import (
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// The per-invocation dependency delegates to the real borrowed journal view.
// It permits operation counting without changing lock ownership or the budget.
func (b *streamBudget) eventAt(view bus.JournalView, epoch string, sequence uint64) (model.Event, error) {
	if b.eventRead != nil {
		return b.eventRead(view, epoch, sequence)
	}
	return view.EventAt(epoch, sequence)
}
