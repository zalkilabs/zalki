package bus

import (
	"encoding/json"
	"github.com/zalkilabs/zalki/internal/journal"
	"github.com/zalkilabs/zalki/internal/model"
)

// Persistence alone adds this field. model.Message deliberately has json:"-"
// so every ordinary JSON projection, including future ones, omits it.
type storedPluginMessage struct {
	model.Message
	Binding string `json:"plugin_binding,omitempty"`
}

func decodeStoredMessage(raw []byte, message *model.Message) error {
	var disk storedPluginMessage
	if err := DecodeStrictRecordJSON(raw, &disk); err != nil {
		return err
	}
	*message = disk.Message
	message.PluginBinding = disk.Binding
	return nil
}

func checkStoredPluginBinding(prior []byte, next model.Message) error {
	if !model.IsPluginInboxAddress(next.From) && next.PluginBinding != "" {
		return journal.ErrInvalidBatch
	}
	if prior == nil {
		return nil
	}
	var before storedPluginMessage
	if err := json.Unmarshal(prior, &before); err != nil {
		return err
	}
	if before.Binding != next.PluginBinding {
		return journal.ErrBatchConflict
	}
	return nil
}
