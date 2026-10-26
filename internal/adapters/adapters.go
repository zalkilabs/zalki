// Package adapters implements the concrete agent clients that plug into the bus.
// It depends on bus; bus never imports it. The composition root builds a
// registry with Builtin and injects it.
package adapters

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zalkilabs/zalki/internal/bus"
)

// Builtin returns a registry of every compiled-in adapter.
func Builtin() *bus.Registry {
	r, err := bus.NewRegistry(codexAdapter{}, claudeAdapter{}, bus.CursorAdapter())
	if err != nil {
		panic(err)
	}
	return r
}

// failedReceipt reports an unsent or failed delivery, keeping err as the live cause.
func failedReceipt(status bus.DeliveryStatus, err error) bus.Receipt {
	receipt := bus.NewReceipt(status)
	receipt.Error = err.Error()
	return receipt.WithCause(err)
}

// configDir returns the absolute directory named by variable, or fallback.
func configDir(variable, fallback string) string {
	if value := os.Getenv(variable); strings.TrimSpace(value) != "" {
		if path, err := filepath.Abs(value); err == nil {
			return path
		}
	}
	return fallback
}

var clientVersionPattern = regexp.MustCompile(`[0-9]+\.[0-9]+\.[0-9]+(?:-[A-Za-z0-9.-]+)?`)
