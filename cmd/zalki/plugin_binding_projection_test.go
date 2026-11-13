package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/zalkilabs/zalki/internal/bus"
	"github.com/zalkilabs/zalki/internal/model"
)

func TestSessionPluginCLIBindingJSONProjection(t *testing.T) {
	message := model.Message{ID: model.NewUUID(), From: "plugin:" + model.NewUUID(), To: "cursor:" + model.NewUUID(), Body: "CLI projection fixture", PluginBinding: strings.Repeat("e", 64)}
	for _, value := range []any{message, bus.MessageBatch{Messages: []model.Message{message}}} {
		cmd := &cobra.Command{}
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := encodeOutput(cmd, value); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(out.Bytes(), []byte("plugin_binding")) || bytes.Contains(out.Bytes(), []byte(message.PluginBinding)) {
			t.Fatal("Z6_CLI_BINDING_NAME_VALUE_REDACTED")
		}
		if !bytes.Contains(out.Bytes(), []byte(message.Body)) {
			t.Fatal("Z6_CLI_PROJECTION_CONTROL_HAS_CONTENT")
		}
	}
}
