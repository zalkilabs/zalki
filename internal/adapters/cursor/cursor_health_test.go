package cursor

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/model"
)

type healthStatusHost struct{ status model.ListenerStatus }

func (h healthStatusHost) Listener(model.Session) model.ListenerStatus { return h.status }

func TestHealthKeepsUnarmedAndUnknownListenerOutput(t *testing.T) {
	for _, test := range []struct {
		name   string
		status model.ListenerStatus
	}{
		{"none", model.ListenerNone},
		{"unknown", model.ListenerStatus("unknown")},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport, listener := (Adapter{}).Health(healthStatusHost{status: test.status}, model.Session{})
			if transport != "deferred" || listener != "not_armed" {
				t.Fatalf("health=(%q, %q), want (deferred, not_armed)", transport, listener)
			}
		})
	}
}
