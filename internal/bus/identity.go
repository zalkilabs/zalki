package bus

import "github.com/zalkilabs/zalki/internal/model"

func newID() string { return model.NewUUID() }
