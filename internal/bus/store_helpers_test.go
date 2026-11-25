package bus

import (
	"github.com/zalkilabs/zalki/internal/fsutil"
	"github.com/zalkilabs/zalki/internal/model"
)

func writeJSON(path string, value any) error {
	b, err := encodeStateJSON(value)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(path, b)
}

func (w commitWriter) commit(previous []byte, next model.Session, event model.Event) error {
	batch, err := w.prepare(previous, next, event)
	if err != nil {
		return err
	}
	return w.log().Commit(batch)
}

func sessionAllowsPlugin(session model.Session, name string) bool {
	p, ok := decodeSessionPluginPolicy(session)
	return sessionPolicyAllowsPlugin(p, ok, name)
}
