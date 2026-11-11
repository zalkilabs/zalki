package bus

import "github.com/zalkilabs/zalki/internal/model"

type sessionRecord struct {
	path  string
	value model.Session
}

func prepareSessionRecord(s Store, v model.Session) (sessionRecord, error) {
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
	if expected, err := s.sessionID(v.Kind, v.NativeID); err != nil || expected != v.ID {
		return sessionRecord{}, model.NewError(model.CodeInvalidIdentity, "native and Zalki session identities do not match")
	}
	return sessionRecord{path: path, value: v}, nil
}
