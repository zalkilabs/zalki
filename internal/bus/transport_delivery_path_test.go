package bus_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

// transportRow sends one message whose adapter returns receipt and returns the
// transport.completed log row and the stored message JSON.
func transportRow(t *testing.T, receipt model.Receipt) (map[string]any, string) {
	t.Helper()
	target := nativeAdapter("beta")
	target.script(func(context.Context, model.Message, model.Session) model.Receipt { return receipt })
	store := newStore(t, nativeAdapter("alpha"), target)
	from, to := register(t, store, "alpha"), register(t, store, "beta")
	var logs bytes.Buffer
	store = store.With(bus.WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))))
	msg, err := (&bus.Service{Store: store}).Send(t.Context(), bus.SendRequest{Actor: from.ID, Capability: from.Key, To: to.ID, Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(&logs)
	for {
		var row map[string]any
		if err := decoder.Decode(&row); errors.Is(err, io.EOF) {
			t.Fatal("transport.completed log was not recorded")
		} else if err != nil {
			t.Fatal(err)
		}
		if row["msg"] == "transport.completed" {
			return row, string(stored)
		}
	}
}

func TestTransportLogNamesTheDeliveryPathOnly(t *testing.T) {
	for _, path := range []string{"app_server_tool_output", "native_queue_next_turn"} {
		row, stored := transportRow(t, model.NewReceipt(model.StatusSubmitted).WithPath(path))
		if row["delivery_path"] != path || row["receipt_status"] != string(model.StatusSubmitted) {
			t.Fatalf("transport log = %v, want delivery_path %s", row, path)
		}
		if bytes.Contains([]byte(stored), []byte("delivery_path")) || bytes.Contains([]byte(stored), []byte(path)) {
			t.Fatalf("delivery path reached the message result: %s", stored)
		}
	}
	if row, _ := transportRow(t, model.NewReceipt(model.StatusSubmitted)); row["delivery_path"] != nil {
		t.Fatalf("an adapter that names no path got delivery_path=%v", row["delivery_path"])
	}
}
