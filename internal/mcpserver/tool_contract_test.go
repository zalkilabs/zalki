package mcpserver

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zalkilabs/zalki/internal/bus"
)

// The full advertised tools/list contract was captured before moving the tool
// table out of bus. Guard content as well as size during behavior-only refactors.
// Exclude the result envelope: its serverInfo version contains a dynamic build ID.
func TestAdvertisedToolContractUnchanged(t *testing.T) {
	client, ctx := ergonomicsClient(t, &bus.Service{})
	result, err := client.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(result.Tools)
	if err != nil {
		t.Fatal(err)
	}
	const want = "ff796639b42b259a19023d867994a45006725d43eb47115936b3b196b098e539"
	if got := fmt.Sprintf("%x", sha256.Sum256(encoded)); got != want {
		t.Fatalf("advertised tool contract changed: sha256=%s bytes=%d; compare with the pre-refactor contract before updating", got, len(encoded))
	}
}
