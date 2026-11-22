package mcpserver

import (
	"os"
	"testing"
	"time"
)

// TestMain fixes the zone once, with an offset, so a time shown "locally" cannot
// be mistaken for UTC and the zone of the machine cannot change any result.
func TestMain(m *testing.M) {
	time.Local = time.FixedZone("test+02:00", 2*60*60)
	os.Exit(m.Run())
}
