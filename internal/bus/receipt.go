package bus

import "github.com/zalkilabs/zalki/internal/model"

// Receipt is the compatibility name for model.Receipt.
type Receipt = model.Receipt

// NewReceipt starts a receipt for status, stamped with the current time.
func NewReceipt(status DeliveryStatus) Receipt { return model.NewReceipt(status) }
