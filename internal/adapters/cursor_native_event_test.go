package adapters_test

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zalkilabs/zalki/internal/adapters/cursor"
	"github.com/zalkilabs/zalki/internal/agent"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// nativeRecorder is the real Cursor adapter that records the event names its
// handler receives, then lets the real handler run.
type nativeRecorder struct {
	cursor.Adapter
	seen *[]string
}

func (a nativeRecorder) HandleHook(host agent.HookHost, event agent.HookInput, id string, output io.Writer) (bool, error) {
	*a.seen = append(*a.seen, event.Event+"/"+event.NativeEvent)
	return a.Adapter.HandleHook(host, event, id, output)
}

// Cursor reports an aborted turn as a stop with a status, and its adapter turns
// that into the common event Interrupt. Core still tells the adapter what the
// client sent: the native event stays "stop" for both.
func TestCursorStopKeepsItsNativeEventWhenItBecomesAnInterrupt(t *testing.T) {
	isolatedClientRoots(t)
	var seen []string
	registry, err := agent.NewRegistry(nativeRecorder{seen: &seen})
	if err != nil {
		t.Fatal(err)
	}
	store, err := bus.NewStore(filepath.Join(t.TempDir(), "state"), registry, nil)
	if err != nil {
		t.Fatal(err)
	}
	session, _, err := store.Register("cursor", model.NewUUID(), "/workspace", nil)
	if err != nil {
		t.Fatal(err)
	}
	service := bus.Service{Store: store}
	for _, status := range []string{"aborted", "completed"} {
		input, err := json.Marshal(map[string]any{"conversation_id": session.NativeID, "hook_event_name": "stop", "status": status})
		if err != nil {
			t.Fatal(err)
		}
		if err := service.Hook(t.Context(), "cursor", bytes.NewReader(input), io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"Interrupt/stop", "Stop/stop"}; !reflect.DeepEqual(seen, want) {
		t.Fatalf("handler calls = %q, want %q", seen, want)
	}
}
