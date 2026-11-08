package bus

import (
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/model"
)

type sessionRecord struct {
	path    string
	value   model.Session
	removed []string
}

func prepareSessionRecord(s Store, v model.Session) (sessionRecord, error) {
	if v.Schema < 0 || v.Schema > leaseSessionSchema {
		return sessionRecord{}, model.NewError(model.CodeUnsupportedSchema, "unsupported session schema %d", v.Schema)
	}
	path, err := s.sessionPath(v.ID)
	if err != nil {
		return sessionRecord{}, err
	}
	adapter, err := s.registry().Lookup(v.Kind)
	if err != nil {
		return sessionRecord{}, err
	}
	v.Receive = adapter.ReceiveCapabilities(v)
	v.Schema = recordSchema
	if v.Listener != nil {
		v.Schema = leaseSessionSchema
	}
	if v.NativeID == "" {
		v.NativeID = model.NativeID(v)
	}
	if expected, err := s.sessionID(v.Kind, v.NativeID); err != nil || expected != v.ID {
		return sessionRecord{}, model.NewError(model.CodeInvalidIdentity, "native and Zalki session identities do not match")
	}
	var removed []string
	if migration, ok := adapter.(agent.SessionStateMigrationAdapter); ok {
		for _, name := range migration.LegacyStateFields() {
			if !coreField(name) {
				removed = append(removed, name)
			}
		}
	}
	return sessionRecord{path: path, value: v, removed: removed}, nil
}
