package adapters_test

import (
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// A native event name that a client does not declare is refused, and nothing
// changes: no output, no new session, no change to a known one. This holds for
// a name nobody declares, for the same name in another case or with a trailing
// space, and for the empty name.
func TestBuiltInClientsRefuseNativeEventsTheyDoNotDeclare(t *testing.T) {
	isolatedClientRoots(t)
	for _, kind := range []string{"claude", "codex", "cursor"} {
		for _, event := range []string{"UnknownEvent", "unknownevent", "Stop ", "PostToolUse2", ""} {
			store := adapterTestStore(t)
			service := bus.Service{Store: store}
			known, _ := registerAdapterSession(t, store, kind)
			before := adapterTestSession(t, store, known.ID)
			for name, native := range map[string]string{"known session": known.NativeID, "unknown session": model.NewUUID()} {
				out, err := hookStdout(t, service, kind, hookPayload(kind, native, event))
				if err == nil || out != "" {
					t.Errorf("%s %q (%s): stdout %q, error %v; want a refusal and no output", kind, event, name, out, err)
				}
			}
			sessions, err := store.List()
			if err != nil || len(sessions) != 1 {
				t.Errorf("%s %q: %d sessions after the refused hooks (%v), want 1", kind, event, len(sessions), err)
			}
			if after := adapterTestSession(t, store, known.ID); after.State != before.State || after.UpdatedAt != before.UpdatedAt || after.Retired != before.Retired {
				t.Errorf("%s %q changed the known session: %+v -> %+v", kind, event, before, after)
			}
		}
	}
}
