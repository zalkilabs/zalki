package mcpserver

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionPluginMCPBindingProjections(t *testing.T) {
	message := model.Message{ID: model.NewUUID(), From: "plugin:" + model.NewUUID(), To: "cursor:" + model.NewUUID(), Body: "projection fixture body", CreatedAt: model.Timestamp(), Status: model.StatusPendingActivity, PluginBinding: strings.Repeat("d", 64)}
	receipt, err := messageReceipt(message, nil)
	if err != nil {
		t.Fatal(err)
	}
	batch := bus.MessageBatch{Messages: []model.Message{message}}
	views := map[string]any{"list": messagePage(batch, false), "get": messagePage(batch, true), "receipt": receipt}
	for name, value := range views {
		raw, err := json.Marshal(value)
		if err != nil || bytes.Contains(raw, []byte("plugin_binding")) || bytes.Contains(raw, []byte(message.PluginBinding)) {
			t.Fatal("Z6_MCP_BINDING_NAME_VALUE_REDACTED", name, err)
		}
		if name != "receipt" && !bytes.Contains(raw, []byte(message.Body)) {
			t.Fatal("Z6_MCP_PROJECTION_CONTROL_HAS_CONTENT", name)
		}
	}
}
