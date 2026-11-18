package pluginfeed

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestStreamPublishedGapEnumsMatchProductionRegistry(t *testing.T) {
	raw, err := os.ReadFile("testdata/plugin-feed/gap-enums.json")
	if err != nil {
		t.Fatal(err)
	}
	var published map[string][]streamEnum
	if err := json.Unmarshal(raw, &published); err != nil {
		t.Fatal(err)
	}
	if len(published) != 1 || !reflect.DeepEqual(published["gap"], streamGapEnums) {
		t.Fatal("F_WIRE_GAP_ENUM_REGISTRY_COMPLETE_GOLDEN")
	}
	for _, entry := range streamGapEnums {
		if entry.Meaning == "" || entry.Context != "gap" {
			t.Fatal("F_WIRE_GAP_ENUM_MEANING_AND_CONTEXT")
		}
	}
}
