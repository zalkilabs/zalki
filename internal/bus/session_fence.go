package bus

import (
	"errors"
	"os"

	"github.com/zalkilabs/zalki/internal/model"
)

var errStaleHook = errors.New("hook registration changed")

// sessionFence is the registration a hook captured before it acted: the
// session's actor, agent kind and generation. A store function that takes one
// refuses a session whose registration has changed since. An unbound fence
// (bound false) and no fence at all mean an ordinary lookup.
type sessionFence struct {
	actor, kind, generation string
	bound                   bool
}

// fenceOf is the registration a hook host captured.
func fenceOf(h hookRuntime) sessionFence {
	return sessionFence{actor: h.actor, kind: h.kind, generation: h.generation, bound: h.generationBound}
}

// Called only inside the operation's existing journal lock. A bound missing
// snapshot never adopts a registration created by another hook later.
func sessionUnderFence(s Store, actor string, fences ...sessionFence) (model.Session, error) {
	v, err := s.session(actor)
	if len(fences) == 0 || !fences[0].bound {
		return v, err
	}
	f := fences[0]
	if errors.Is(err, os.ErrNotExist) {
		return v, errStaleHook
	}
	if err != nil {
		return v, err
	}
	if f.actor != actor || v.ID != actor || v.Kind != f.kind || f.generation == "" || v.Generation != f.generation {
		return v, errStaleHook
	}
	return v, nil
}
